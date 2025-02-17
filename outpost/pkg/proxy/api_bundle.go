package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jinzhu/copier"
	"github.com/justinas/alice"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/apis/options"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/middleware"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/validation"
	log "github.com/sirupsen/logrus"
	"goauthentik.io/outpost/api"
	"goauthentik.io/outpost/pkg/ak"
)

type providerBundle struct {
	http.Handler

	s     *Server
	proxy *OAuthProxy
	Host  string

	endSessionUrl string

	cert *tls.Certificate

	log *log.Entry
}

func intToPointer(i int) *int {
	return &i
}

func (pb *providerBundle) prepareOpts(provider api.ProxyOutpostConfig) *options.Options {
	externalHost, err := url.Parse(provider.ExternalHost)
	if err != nil {
		log.WithError(err).Warning("Failed to parse URL, skipping provider")
		return nil
	}
	providerOpts := &options.Options{}
	err = copier.Copy(&providerOpts, getCommonOptions())
	if err != nil {
		log.WithError(err).Warning("Failed to copy options, skipping provider")
		return nil
	}
	providerOpts.ClientID = *provider.ClientId
	providerOpts.ClientSecret = *provider.ClientSecret

	providerOpts.Cookie.Secret = *provider.CookieSecret
	providerOpts.Cookie.Secure = externalHost.Scheme == "https"

	providerOpts.SkipOIDCDiscovery = true
	providerOpts.OIDCIssuerURL = provider.OidcConfiguration.Issuer
	providerOpts.LoginURL = provider.OidcConfiguration.AuthorizationEndpoint
	providerOpts.RedeemURL = provider.OidcConfiguration.TokenEndpoint
	providerOpts.OIDCJwksURL = provider.OidcConfiguration.JwksUri
	providerOpts.ProfileURL = provider.OidcConfiguration.UserinfoEndpoint
	providerOpts.ValidateURL = provider.OidcConfiguration.UserinfoEndpoint
	providerOpts.AcrValues = "goauthentik.io/providers/oauth2/default"

	if *provider.SkipPathRegex != "" {
		skipRegexes := strings.Split(*provider.SkipPathRegex, "\n")
		providerOpts.SkipAuthRegex = skipRegexes
	}

	if *provider.Mode == api.PROXYMODE_FORWARD_SINGLE || *provider.Mode == api.PROXYMODE_FORWARD_DOMAIN {
		providerOpts.UpstreamServers = []options.Upstream{
			{
				ID:         "static",
				Static:     true,
				StaticCode: intToPointer(202),
				Path:       "/",
			},
		}
	} else {
		providerOpts.UpstreamServers = []options.Upstream{
			{
				ID:                    "default",
				URI:                   *provider.InternalHost,
				Path:                  "/",
				InsecureSkipTLSVerify: !(*provider.InternalHostSslValidation),
			},
		}
	}

	if provider.Certificate.Get() != nil {
		pb.log.WithField("provider", provider.Name).Debug("Enabling TLS")
		cert, err := ak.ParseCertificate(*provider.Certificate.Get(), pb.s.ak.Client.CryptoApi)
		if err != nil {
			pb.log.WithField("provider", provider.Name).WithError(err).Warning("Failed to fetch certificate")
			return providerOpts
		}
		pb.cert = cert
		pb.log.WithField("provider", provider.Name).Debug("Loaded certificates")
	}
	return providerOpts
}

func (pb *providerBundle) Build(provider api.ProxyOutpostConfig) {
	opts := pb.prepareOpts(provider)

	if *provider.Mode == api.PROXYMODE_FORWARD_DOMAIN {
		opts.Cookie.Domains = []string{*provider.CookieDomain}
	}

	chain := alice.New()

	if opts.ForceHTTPS {
		_, httpsPort, err := net.SplitHostPort(opts.HTTPSAddress)
		if err != nil {
			log.Fatalf("FATAL: invalid HTTPS address %q: %v", opts.HTTPAddress, err)
		}
		chain = chain.Append(middleware.NewRedirectToHTTPS(httpsPort))
	}

	healthCheckPaths := []string{opts.PingPath}
	healthCheckUserAgents := []string{opts.PingUserAgent}

	// To silence logging of health checks, register the health check handler before
	// the logging handler
	if opts.Logging.SilencePing {
		chain = chain.Append(middleware.NewHealthCheck(healthCheckPaths, healthCheckUserAgents), LoggingHandler)
	} else {
		chain = chain.Append(LoggingHandler, middleware.NewHealthCheck(healthCheckPaths, healthCheckUserAgents))
	}

	err := validation.Validate(opts)
	if err != nil {
		log.Printf("%s", err)
		os.Exit(1)
	}
	oauthproxy, err := NewOAuthProxy(opts, provider, pb.s.ak.Client.GetConfig().HTTPClient)
	if err != nil {
		log.Errorf("ERROR: Failed to initialise OAuth2 Proxy: %v", err)
		os.Exit(1)
	}

	if *provider.BasicAuthEnabled {
		oauthproxy.SetBasicAuth = true
		oauthproxy.BasicAuthUserAttribute = *provider.BasicAuthUserAttribute
		oauthproxy.BasicAuthPasswordAttribute = *provider.BasicAuthPasswordAttribute
	}

	oauthproxy.endSessionEndpoint = pb.endSessionUrl
	oauthproxy.ExternalHost = pb.Host

	pb.proxy = oauthproxy
	pb.Handler = chain.Then(oauthproxy)
}
