// Teleport
// Copyright (C) 2024 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package vnet

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/singleflight"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/gen/proto/go/teleport/vnet/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/srv/alpnproxy"
	alpncommon "github.com/gravitational/teleport/lib/srv/alpnproxy/common"
)

// AppProvider is an interface providing the necessary methods to log in to apps and get clients able to list
// apps in all clusters in all current profiles. This should be the minimum necessary interface that needs to
// be implemented differently for Connect and `tsh vnet`.
type AppProvider interface {
	// ListProfiles lists the names of all profiles saved for the user.
	ListProfiles() ([]string, error)

	// GetCachedClient returns a [*client.ClusterClient] for the given profile and leaf cluster.
	// [leafClusterName] may be empty when requesting a client for the root cluster. Returned clients are
	// expected to be cached, as this may be called frequently.
	GetCachedClient(ctx context.Context, profileName, leafClusterName string) (ClusterClient, error)

	// ReissueAppCert returns a new app certificate for the given app in the named profile and leaf cluster.
	// Implementations may trigger a re-login to the cluster, but if they do, they MUST clear all cached
	// clients for that cluster so that new working clients will be returned from [GetCachedClient].
	ReissueAppCert(ctx context.Context, profileName, leafClusterName string, app types.Application) (tls.Certificate, error)

	// GetDialOptions returns ALPN dial options for the profile.
	GetDialOptions(ctx context.Context, profileName string) (*DialOptions, error)

	// GetVnetConfig returns the cluster VnetConfig resource.
	GetVnetConfig(ctx context.Context, profileName, leafClusterName string) (*vnet.VnetConfig, error)

	// OnNewConnection gets called whenever a new connection is about to be established through VNet.
	// By the time OnNewConnection, VNet has already verified that the user holds a valid cert for the
	// app.
	//
	// The connection won't be established until OnNewConnection returns. Returning an error prevents
	// the connection from being made.
	OnNewConnection(ctx context.Context, profileName, leafClusterName string, app types.Application) error
}

// ClusterClient is an interface defining the subset of [client.ClusterClient] methods used by [AppProvider].
type ClusterClient interface {
	CurrentCluster() authclient.ClientI
	ClusterName() string
}

// DialOptions holds ALPN dial options for dialing apps.
type DialOptions struct {
	// WebProxyAddr is the address to dial.
	WebProxyAddr string
	// ALPNConnUpgradeRequired specifies if ALPN connection upgrade is required.
	ALPNConnUpgradeRequired bool
	// SNI is a ServerName value set for upstream TLS connection.
	SNI string
	// RootClusterCACertPool overrides the x509 certificate pool used to verify the server.
	RootClusterCACertPool *x509.CertPool
	// InsecureSkipTLSVerify turns off verification for x509 upstream ALPN proxy service certificate.
	InsecureSkipVerify bool
}

// TCPAppResolver implements [TCPHandlerResolver] for Teleport TCP apps.
type TCPAppResolver struct {
	appProvider          AppProvider
	clusterConfigCache   *clusterConfigCache
	customDNSZoneChecker *customDNSZoneValidator
	slog                 *slog.Logger
	clock                clockwork.Clock
	lookupTXT            lookupTXTFunc
}

// NewTCPAppResolver returns a new *TCPAppResolver which will resolve full-qualified domain names to
// TCPHandlers that will proxy TCP connection to Teleport TCP apps.
//
// It uses [appProvider] to list and retrieve cluster clients which are expected to be cached to avoid
// repeated/unnecessary dials to the cluster. These clients are then used to list TCP apps that should be
// handled.
//
// [appProvider] is also used to get app certificates used to dial the apps.
func NewTCPAppResolver(appProvider AppProvider, opts ...tcpAppResolverOption) (*TCPAppResolver, error) {
	r := &TCPAppResolver{
		appProvider: appProvider,
		slog:        slog.With(teleport.ComponentKey, "VNet.AppResolver"),
	}
	for _, opt := range opts {
		opt(r)
	}
	r.clock = cmp.Or(r.clock, clockwork.NewRealClock())
	r.clusterConfigCache = newClusterConfigCache(appProvider.GetVnetConfig, r.clock)
	r.customDNSZoneChecker = newCustomDNSZoneValidator(r.lookupTXT)
	return r, nil
}

type tcpAppResolverOption func(*TCPAppResolver)

// withClock is a functional option to override the default clock (for tests).
func withClock(clock clockwork.Clock) tcpAppResolverOption {
	return func(r *TCPAppResolver) {
		r.clock = clock
	}
}

// withLookupTXTFunc is a functional option to override the DNS TXT record lookup function (for tests).
func withLookupTXTFunc(lookupTXT lookupTXTFunc) tcpAppResolverOption {
	return func(r *TCPAppResolver) {
		r.lookupTXT = lookupTXT
	}
}

// ResolveTCPHandler resolves a fully-qualified domain name to a [TCPHandlerSpec] for a Teleport TCP app that should
// be used to handle all future TCP connections to [fqdn].
// Avoid using [trace.Wrap] on [ErrNoTCPHandler] to prevent collecting a full stack trace on every unhandled
// query.
func (r *TCPAppResolver) ResolveTCPHandler(ctx context.Context, fqdn string) (*TCPHandlerSpec, error) {
	profileNames, err := r.appProvider.ListProfiles()
	if err != nil {
		return nil, trace.Wrap(err, "listing profiles")
	}
	for _, profileName := range profileNames {
		if fqdn == fullyQualify(profileName) {
			// This is a query for the proxy address, which we'll never want to handle.
			return nil, ErrNoTCPHandler
		}
		if match, err := r.fqdnMatchesProfile(ctx, profileName, fqdn); err != nil {
			return nil, trace.Wrap(err)
		} else if !match {
			continue
		}

		slog := r.slog.With("profile", profileName, "fqdn", fqdn)
		rootClient, err := r.appProvider.GetCachedClient(ctx, profileName, "")
		if err != nil {
			// The user might be logged out from this one cluster (and retryWithRelogin isn't working). Don't
			// return an error so that DNS resolution will be forwarded upstream instead of failing, to avoid
			// breaking e.g. web app access (we don't know if this is a web or TCP app yet because we can't
			// log in).
			slog.ErrorContext(ctx, "Failed to get teleport client.", "error", err)
			continue
		}
		return r.resolveTCPHandlerForCluster(ctx, slog, rootClient.CurrentCluster(), profileName, "", fqdn)
	}
	// fqdn did not match any profile, forward the request upstream.
	return nil, ErrNoTCPHandler
}

func (r *TCPAppResolver) fqdnMatchesProfile(ctx context.Context, profileName, fqdn string) (bool, error) {
	if isSubdomain(fqdn, profileName) {
		// The queried app fqdn is a subdomain of the proxy address, this is a match.
		return true, nil
	}
	// Not a proxy address subdomain, must check custom DNS zones.

	// TODO(nklaassen): support leaf clusters.
	vnetConfig, err := r.clusterConfigCache.getVnetConfig(ctx, profileName, "" /*leafClustername*/)
	if err != nil {
		// Good chance we're here because the user is not logged in to the profile.
		r.slog.ErrorContext(ctx, "Failed to get VnetConfig, not checking custom DNS zones.", "profile", profileName, "error", err)
		return false, nil
	}

	// TODO(nklaassen): support leaf clusters.
	rootClient, err := r.appProvider.GetCachedClient(ctx, profileName, "")
	if err != nil {
		r.slog.ErrorContext(ctx, "Failed to get teleport client, not checking custom DNS zones.", "profile", profileName, "error", err)
		return false, nil
	}
	clusterName := rootClient.ClusterName()
	for _, zone := range vnetConfig.GetSpec().GetCustomDnsZones() {
		if !isSubdomain(fqdn, zone.GetSuffix()) {
			// The queried app fqdn is not a subdomain of this custom zone suffix, skip it.
			continue
		}
		// The queried app fqdn is a subdomain of this custom zone suffix. Check if the custom zone is valid.
		if err := r.customDNSZoneChecker.validate(ctx, clusterName, zone.GetSuffix()); err != nil {
			r.slog.ErrorContext(ctx, "Failed to validate custom DNS zone %q for cluster %q", "error", err)
			return false, trace.Wrap(err, "validating custom DNS zone")
		}
		return true, nil
	}
	return false, nil
}

// resolveTCPHandlerForCluster takes a cluster client and resolves [fqdn] to a [TCPHandlerSpec] if a matching
// app is found in that cluster.
// Avoid using [trace.Wrap] on [ErrNoTCPHandler] to prevent collecting a full stack trace on every unhandled
// query.
func (r *TCPAppResolver) resolveTCPHandlerForCluster(
	ctx context.Context,
	slog *slog.Logger,
	clt apiclient.GetResourcesClient,
	profileName, leafClusterName, fqdn string,
) (*TCPHandlerSpec, error) {
	// An app public_addr could technically be full-qualified or not, match either way.
	expr := fmt.Sprintf(`(resource.spec.public_addr == "%s" || resource.spec.public_addr == "%s") && hasPrefix(resource.spec.uri, "tcp://")`,
		strings.TrimSuffix(fqdn, "."), fqdn)
	resp, err := apiclient.GetResourcePage[types.AppServer](ctx, clt, &proto.ListResourcesRequest{
		ResourceType:        types.KindAppServer,
		PredicateExpression: expr,
		Limit:               1,
	})
	if err != nil {
		// Don't return an unexpected error so we can try to find the app in different clusters or forward the
		// request upstream.
		slog.InfoContext(ctx, "Failed to list application servers.", "error", err)
		return nil, ErrNoTCPHandler
	}
	if len(resp.Resources) == 0 {
		// Didn't find any matching app, forward the request upstream.
		return nil, ErrNoTCPHandler
	}
	app := resp.Resources[0].GetApp()
	appHandler, err := r.newTCPAppHandler(ctx, profileName, leafClusterName, app)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var cidrRange string
	vnetConfig, err := r.clusterConfigCache.getVnetConfig(ctx, profileName, leafClusterName)
	switch {
	case err == nil:
		cidrRange = cmp.Or(vnetConfig.GetSpec().GetIpv4CidrRange(), defaultIPv4CIDRRange)
	case trace.IsNotFound(err) || trace.IsNotImplemented(err):
		cidrRange = defaultIPv4CIDRRange
	default:
		return nil, trace.Wrap(err)
	}

	return &TCPHandlerSpec{
		IPv4CIDRRange: cidrRange,
		TCPHandler:    appHandler,
	}, nil
}

type tcpAppHandler struct {
	profileName     string
	leafClusterName string
	app             types.Application
	lp              *alpnproxy.LocalProxy
}

func (r *TCPAppResolver) newTCPAppHandler(
	ctx context.Context,
	profileName string,
	leafClusterName string,
	app types.Application,
) (*tcpAppHandler, error) {
	dialOpts, err := r.appProvider.GetDialOptions(ctx, profileName)
	if err != nil {
		return nil, trace.Wrap(err, "getting dial options for profile %q", profileName)
	}

	appCertIssuer := &appCertIssuer{
		appProvider:     r.appProvider,
		profileName:     profileName,
		leafClusterName: leafClusterName,
		app:             app,
	}
	certChecker := client.NewCertChecker(appCertIssuer, r.clock)
	middleware := &localProxyMiddleware{
		certChecker:     certChecker,
		appProvider:     r.appProvider,
		app:             app,
		profileName:     profileName,
		leafClusterName: leafClusterName,
	}

	localProxyConfig := alpnproxy.LocalProxyConfig{
		RemoteProxyAddr:         dialOpts.WebProxyAddr,
		Protocols:               []alpncommon.Protocol{alpncommon.ProtocolTCP},
		ParentContext:           ctx,
		SNI:                     dialOpts.SNI,
		RootCAs:                 dialOpts.RootClusterCACertPool,
		ALPNConnUpgradeRequired: dialOpts.ALPNConnUpgradeRequired,
		Middleware:              middleware,
		InsecureSkipVerify:      dialOpts.InsecureSkipVerify,
		Clock:                   r.clock,
	}

	lp, err := alpnproxy.NewLocalProxy(localProxyConfig)
	if err != nil {
		return nil, trace.Wrap(err, "creating local proxy")
	}

	return &tcpAppHandler{
		profileName:     profileName,
		leafClusterName: leafClusterName,
		app:             app,
		lp:              lp,
	}, nil
}

// HandleTCPConnector handles an incoming TCP connection from VNet by passing it to the local alpn proxy,
// which is set up with middleware to automatically handler certificate renewal and re-logins.
func (h *tcpAppHandler) HandleTCPConnector(ctx context.Context, connector func() (net.Conn, error)) error {
	return trace.Wrap(h.lp.HandleTCPConnector(ctx, connector), "handling TCP connector")
}

// appCertIssuer implements [client.CertIssuer].
type appCertIssuer struct {
	appProvider     AppProvider
	profileName     string
	leafClusterName string
	app             types.Application
	group           singleflight.Group
}

func (i *appCertIssuer) CheckCert(cert *x509.Certificate) error {
	// appCertIssuer does not perform any additional certificate checks.
	return nil
}

func (i *appCertIssuer) IssueCert(ctx context.Context) (tls.Certificate, error) {
	cert, err, _ := i.group.Do("", func() (any, error) {
		return i.appProvider.ReissueAppCert(ctx, i.profileName, i.leafClusterName, i.app)
	})
	return cert.(tls.Certificate), trace.Wrap(err)
}

func isSubdomain(appFQDN, suffix string) bool {
	return strings.HasSuffix(appFQDN, "."+fullyQualify(suffix))
}

// fullyQualify returns a fully-qualified domain name from [domain]. Fully-qualified domain names always end
// with a ".".
func fullyQualify(domain string) string {
	if strings.HasSuffix(domain, ".") {
		return domain
	}
	return domain + "."
}

// localProxyMiddleware wraps around [client.CertChecker] and additionally makes it so that its
// OnNewConnection method calls the same method of [AppProvider].
type localProxyMiddleware struct {
	app             types.Application
	profileName     string
	leafClusterName string
	certChecker     *client.CertChecker
	appProvider     AppProvider
}

func (m *localProxyMiddleware) OnNewConnection(ctx context.Context, lp *alpnproxy.LocalProxy) error {
	err := m.certChecker.OnNewConnection(ctx, lp)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(m.appProvider.OnNewConnection(ctx, m.profileName, m.leafClusterName, m.app))
}

func (m *localProxyMiddleware) OnStart(ctx context.Context, lp *alpnproxy.LocalProxy) error {
	return trace.Wrap(m.certChecker.OnStart(ctx, lp))
}
