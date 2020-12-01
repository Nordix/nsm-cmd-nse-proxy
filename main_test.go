// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/cls"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"
	registryapi "github.com/networkservicemesh/api/pkg/api/registry"
	main "github.com/networkservicemesh/cmd-nse-proxy"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/registry"
	"github.com/networkservicemesh/sdk/pkg/tools/callback"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
	"github.com/networkservicemesh/sdk/pkg/tools/spire"

	"github.com/networkservicemesh/sdk/pkg/networkservice/common/excludedprefixes"

	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"

	"github.com/networkservicemesh/sdk/pkg/registry/common/setid"
	"github.com/networkservicemesh/sdk/pkg/registry/common/seturl"
	chain_registry "github.com/networkservicemesh/sdk/pkg/registry/core/chain"
	"github.com/networkservicemesh/sdk/pkg/registry/memory"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/endpoint"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/connect"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/discover"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/localbypass"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/roundrobin"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/adapters"
	adapter_registry "github.com/networkservicemesh/sdk/pkg/registry/core/adapters"
	"github.com/networkservicemesh/sdk/pkg/tools/addressof"
	"github.com/networkservicemesh/sdk/pkg/tools/token"
)

type ProxyNSESuite struct {
	suite.Suite

	ctx        context.Context
	cancel     context.CancelFunc
	spireErrCh <-chan error
}

func (f *ProxyNSESuite) SetupSuite() {
	logrus.SetFormatter(&nested.Formatter{})
	logrus.SetLevel(logrus.TraceLevel)
	f.ctx, f.cancel = context.WithCancel(context.Background())

	// Run spire
	executable, err := os.Executable()
	require.NoError(f.T(), err)

	reuseSpire := os.Getenv(workloadapi.SocketEnv) != ""
	if !reuseSpire {
		f.spireErrCh = spire.Start(
			spire.WithContext(f.ctx),
			spire.WithEntry("spiffe://example.org/proxy-nse", "unix:path:/bin/nsmgr"),
			spire.WithEntry("spiffe://example.org/proxy-nse.test", "unix:uid:0"),
			spire.WithEntry(fmt.Sprintf("spiffe://example.org/%s", filepath.Base(executable)),
				fmt.Sprintf("unix:path:%s", executable),
			),
		)
	}
}
func (f *ProxyNSESuite) TearDownSuite() {
	f.cancel()
	if f.spireErrCh != nil {
		for {
			_, ok := <-f.spireErrCh
			if !ok {
				break
			}
		}
	}
}

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestProxyNSCTestSuite(t *testing.T) {
	suite.Run(t, new(ProxyNSESuite))
}

type nsmgrServer struct {
	endpoint.Endpoint
	registry.Registry
}

func newServer(ctx context.Context, nsmRegistration *registryapi.NetworkServiceEndpoint, authzServer networkservice.NetworkServiceServer, tokenGenerator token.GeneratorFunc, clientDialOptions ...grpc.DialOption) *nsmgrServer {
	rv := &nsmgrServer{}

	var localbypassRegistryServer registryapi.NetworkServiceEndpointRegistryServer

	nsRegistry := memory.NewNetworkServiceRegistryServer()
	nseRegistry := chain_registry.NewNetworkServiceEndpointRegistryServer(
		setid.NewNetworkServiceEndpointRegistryServer(),  // If no remote registry then assign ID.
		memory.NewNetworkServiceEndpointRegistryServer(), // Memory registry to store result inside.
	)
	// Construct Endpoint
	rv.Endpoint = endpoint.NewServer(ctx,
		nsmRegistration.Name,
		authzServer,
		tokenGenerator,
		discover.NewServer(adapter_registry.NetworkServiceServerToClient(nsRegistry),
			adapter_registry.NetworkServiceEndpointServerToClient(nseRegistry)),
		roundrobin.NewServer(),
		localbypass.NewServer(&localbypassRegistryServer),
		excludedprefixes.NewServer(ctx),
		connect.NewServer(
			ctx,
			client.NewClientFactory(nsmRegistration.Name,
				addressof.NetworkServiceClient(
					adapters.NewServerToClient(rv)),
				tokenGenerator),
			clientDialOptions...),
	)

	nsChain := chain_registry.NewNetworkServiceRegistryServer(nsRegistry)
	nseChain := chain_registry.NewNetworkServiceEndpointRegistryServer(
		localbypassRegistryServer, // Store endpoint Id to EndpointURL for local access.
		seturl.NewNetworkServiceEndpointRegistryServer(nsmRegistration.Url), // Remember endpoint URL
		nseRegistry, // Register NSE inside Remote registry with ID assigned
	)
	rv.Registry = registry.NewServer(nsChain, nseChain)

	return rv
}

func (n *nsmgrServer) Register(s *grpc.Server) {
	grpcutils.RegisterHealthServices(s, n, n.NetworkServiceEndpointRegistryServer(), n.NetworkServiceRegistryServer())
	networkservice.RegisterNetworkServiceServer(s, n)
	networkservice.RegisterMonitorConnectionServer(s, n)
	registryapi.RegisterNetworkServiceRegistryServer(s, n.Registry.NetworkServiceRegistryServer())
	registryapi.RegisterNetworkServiceEndpointRegistryServer(s, n.Registry.NetworkServiceEndpointRegistryServer())
}

var _ endpoint.Endpoint = &nsmgrServer{}
var _ registry.Registry = &nsmgrServer{}

type endpointImpl struct {
}

func (e *endpointImpl) Request(ctx context.Context, request *networkservice.NetworkServiceRequest) (*networkservice.Connection, error) {
	if request.Connection.Context == nil {
		request.Connection.Context = &networkservice.ConnectionContext{}
	}
	if request.Connection.Context.ExtraContext == nil {
		request.Connection.Context.ExtraContext = map[string]string{}
	}
	request.Connection.Context.ExtraContext["processed"] = "ok"

	return request.Connection, nil
}

func (e *endpointImpl) Close(ctx context.Context, connection *networkservice.Connection) (*empty.Empty, error) {
	return &empty.Empty{}, nil
}

func (f *ProxyNSESuite) TestProxyEndpoint() {
	t := f.T()
	ctx, cancel := context.WithTimeout(context.Background(), 2000*time.Second)
	defer cancel()

	// Get a X509Source
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	var svid *x509svid.SVID
	svid, err = source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("SVID: %q", svid.ID)

	// Construct callback server
	callbackServer := callback.NewServer(main.IdentityByEndpointID)

	// Start nsmgr
	mgrURL := f.setupManager(ctx, source, callbackServer)

	// Now we have an test nsmgr, and we could register proxy endpoint.
	config := &main.Config{
		Name:             "proxy-endpoint",
		ListenOn:         url.URL{Host: ":0", Scheme: "tcp"},
		ConnectTo:        *mgrURL,
		MaxTokenLifetime: time.Hour * 24,
	}
	require.NoError(t, main.StartNSMProxyEndpoint(ctx, config))

	// Now we have an endpoint up and running, we could connect to it and try perform request

	// Construct a local endpoint and register it
	testEp := &endpointImpl{}

	clientCtx, clientCC := f.setupTestEP(ctx, testEp, config)

	// Register endpoint to proxy endpoint and nsmgr
	registryClient := registryapi.NewNetworkServiceEndpointRegistryClient(clientCC)

	_, err4 := registryapi.NewNetworkServiceRegistryClient(clientCC).Register(clientCtx, &registryapi.NetworkService{
		Name:    "network-service",
		Payload: "ip",
	})

	require.NoError(t, err4)

	_, err3 := registryClient.Register(clientCtx, &registryapi.NetworkServiceEndpoint{
		Name:                "endpoint",
		NetworkServiceNames: []string{"network-service"},
		Url:                 "callback:my_endpoint/client",
	})
	require.NoError(t, err3)

	// Check we had all stuff inside nsmgr.

	findResult, findErr := registryClient.Find(ctx, &registryapi.NetworkServiceEndpointQuery{
		Watch: false, NetworkServiceEndpoint: &registryapi.NetworkServiceEndpoint{
			NetworkServiceNames: []string{"network-service"},
		}})
	require.NoError(t, findErr)
	endpoints := registryapi.ReadNetworkServiceEndpointList(findResult)
	require.NotNil(t, endpoints)
	require.Equal(t, 1, len(endpoints))

	// Construct a normal client and perform request to our endpoint.
	var nsmgrClient *grpc.ClientConn
	nsmgrClient, err = grpc.DialContext(ctx,
		grpcutils.URLToTarget(mgrURL),
		grpc.WithTransportCredentials(
			credentials.NewTLS(
				tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true)))

	require.NoError(t, err)

	cl := client.NewClient(ctx, "nsc", nil, spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime), nsmgrClient)

	var conn *networkservice.Connection
	conn, err = cl.Request(ctx, &networkservice.NetworkServiceRequest{
		Connection: &networkservice.Connection{
			NetworkService: "network-service",
			Context:        &networkservice.ConnectionContext{},
		},
		MechanismPreferences: []*networkservice.Mechanism{
			{
				Cls:  cls.LOCAL,
				Type: kernel.MECHANISM,
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.Equal(t, "ok", conn.Context.ExtraContext["processed"])
}

func (f *ProxyNSESuite) setupManager(ctx context.Context, source *workloadapi.X509Source, callbackServer callback.Server) *url.URL {
	mgr := newServer(ctx, &registryapi.NetworkServiceEndpoint{
		Name: "nsmgr",
		Url:  "",
	}, authorize.NewServer(), spiffejwt.TokenGeneratorFunc(source, time.Hour),
		callbackServer.WithCallbackDialer(),

		// Security to connect to endpoint
		grpc.WithTransportCredentials(
			credentials.NewTLS(
				tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true)))

	// Construct a manager listening on random tcp port
	mgrURL := &url.URL{Scheme: "tcp", Host: ":0"}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny()))))
	mgr.Register(server)

	// Register callback serve to grpc.
	callback.RegisterCallbackServiceServer(server, callbackServer)

	_ = grpcutils.ListenAndServe(ctx, mgrURL, server)
	return mgrURL
}

func TokenGenerator(peerAuthInfo credentials.AuthInfo) (tok string, expireTime time.Time, err error) {
	return "TestToken", time.Date(3000, 1, 1, 1, 1, 1, 1, time.UTC), nil
}

func (f *ProxyNSESuite) setupTestEP(ctx context.Context, testEp networkservice.NetworkServiceServer, config *main.Config) (context.Context, *grpc.ClientConn) {
	ep := endpoint.NewServer(ctx, "test-ep", authorize.NewServer(), TokenGenerator, testEp)

	epSrv := grpc.NewServer()
	ep.Register(epSrv)

	clientCtx := main.WithCallbackEndpointID(ctx, "my_endpoint/client")

	// Construct connection to Proxy endpoint
	clientCC, err2 := grpc.DialContext(clientCtx, grpcutils.URLToTarget(&config.ListenOn), grpc.WithInsecure(), grpc.WithDefaultCallOptions(grpc.WaitForReady(true)))
	require.NoError(f.T(), err2)

	// Connect to nsmgr with callback URI
	callbackClient := callback.NewClient(clientCC, epSrv)
	callbackClient.Serve(clientCtx)
	return clientCtx, clientCC
}
