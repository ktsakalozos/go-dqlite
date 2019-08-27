package dqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/Rican7/retry/backoff"
	"github.com/Rican7/retry/strategy"
	"github.com/canonical/go-dqlite/client"
	"github.com/canonical/go-dqlite/internal/bindings"
	"github.com/canonical/go-dqlite/internal/protocol"
	"github.com/pkg/errors"
)

// Node runs a dqlite node.
type Node struct {
	log         client.LogFunc // Logger
	server      *bindings.Node // Low-level C implementation
	acceptCh    chan error     // Receives connection handling errors
	id          uint64
	address     string
	bindAddress string
}

// NodeOption can be used to tweak node parameters.
type NodeOption func(*serverOptions)

// WithNodeLogFunc sets a custom log function for the server.
func WithNodeLogFunc(log client.LogFunc) NodeOption {
	return func(options *serverOptions) {
		options.Log = log
	}
}

// WithNodeDialFunc sets a custom dial function for the server.
func WithNodeDialFunc(dial client.DialFunc) NodeOption {
	return func(options *serverOptions) {
		options.DialFunc = dial
	}
}

// WithBindAddress sets a custom bind address for the server.
func WithNodeBindAddress(address string) NodeOption {
	return func(options *serverOptions) {
		options.BindAddress = address
	}
}

// NewNode creates a new Node instance.
func NewNode(info client.NodeInfo, dir string, options ...NodeOption) (*Node, error) {
	o := defaultNodeOptions()

	for _, option := range options {
		option(o)
	}

	server, err := bindings.NewNode(uint(info.ID), info.Address, dir)
	if err != nil {
		return nil, err
	}
	if o.DialFunc != nil {
		if err := server.SetDialFunc(protocol.DialFunc(o.DialFunc)); err != nil {
			return nil, err
		}
	}
	bindAddress := fmt.Sprintf("@dqlite-%d", info.ID)
	if o.BindAddress != "" {
		bindAddress = o.BindAddress
	}
	if err := server.SetBindAddress(bindAddress); err != nil {
		return nil, err
	}
	s := &Node{
		log:         o.Log,
		server:      server,
		acceptCh:    make(chan error, 1),
		id:          info.ID,
		address:     info.Address,
		bindAddress: bindAddress,
	}

	return s, nil
}

// BindAddress returns the network address the node is listening to.
func (s *Node) BindAddress() string {
	return s.server.GetBindAddress()
}

// Cluster returns information about all servers in the cluster.
func (s *Node) Cluster(ctx context.Context) ([]client.NodeInfo, error) {
	c, err := protocol.Connect(ctx, protocol.UnixDial, s.bindAddress, protocol.VersionLegacy)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to dqlite task")
	}
	defer c.Close()

	request := protocol.Message{}
	request.Init(16)
	response := protocol.Message{}
	response.Init(512)

	protocol.EncodeCluster(&request)

	if err := c.Call(ctx, &request, &response); err != nil {
		return nil, errors.Wrap(err, "failed to send Cluster request")
	}

	servers, err := protocol.DecodeNodes(&response)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse Node response")
	}

	return servers, nil
}

// Leader returns information about the current leader, if any.
func (s *Node) Leader(ctx context.Context) (*client.NodeInfo, error) {
	p, err := protocol.Connect(ctx, protocol.UnixDial, s.bindAddress, protocol.VersionOne)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to dqlite task")
	}
	defer p.Close()

	request := protocol.Message{}
	request.Init(16)
	response := protocol.Message{}
	response.Init(512)

	protocol.EncodeLeader(&request)

	if err := p.Call(ctx, &request, &response); err != nil {
		return nil, errors.Wrap(err, "failed to send Leader request")
	}

	id, address, err := protocol.DecodeNode(&response)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse Node response")
	}

	info := &client.NodeInfo{ID: id, Address: address}

	return info, nil
}

// Start serving requests.
func (s *Node) Start() error {
	return s.server.Start()
}

// Join a cluster.
func (s *Node) Join(ctx context.Context, store client.NodeStore, dial client.DialFunc) error {
	if dial == nil {
		dial = protocol.TCPDial
	}
	config := protocol.Config{
		Dial:           protocol.DialFunc(dial),
		AttemptTimeout: time.Second,
		RetryStrategies: []strategy.Strategy{
			strategy.Backoff(backoff.BinaryExponential(time.Millisecond))},
	}
	connector := protocol.NewConnector(0, store, config, s.log)
	c, err := connector.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	request := protocol.Message{}
	request.Init(4096)
	response := protocol.Message{}
	response.Init(4096)

	protocol.EncodeJoin(&request, s.id, s.address)

	if err := c.Call(ctx, &request, &response); err != nil {
		return err
	}

	protocol.EncodePromote(&request, s.id)

	if err := c.Call(ctx, &request, &response); err != nil {
		return err
	}

	return nil
}

// Leave a cluster.
func Leave(ctx context.Context, id uint64, store client.NodeStore, dial client.DialFunc) error {
	if dial == nil {
		dial = protocol.TCPDial
	}
	config := protocol.Config{
		Dial:           protocol.DialFunc(dial),
		AttemptTimeout: time.Second,
		RetryStrategies: []strategy.Strategy{
			strategy.Backoff(backoff.BinaryExponential(time.Millisecond))},
	}
	connector := protocol.NewConnector(0, store, config, client.DefaultLogFunc())
	c, err := connector.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	request := protocol.Message{}
	request.Init(4096)
	response := protocol.Message{}
	response.Init(4096)

	protocol.EncodeRemove(&request, id)

	if err := c.Call(ctx, &request, &response); err != nil {
		return err
	}

	return nil
}

// Hold configuration options for a dqlite server.
type serverOptions struct {
	Log         client.LogFunc
	DialFunc    client.DialFunc
	BindAddress string
}

// Close the server, releasing all resources it created.
func (s *Node) Close() error {
	// Send a stop signal to the dqlite event loop.
	if err := s.server.Stop(); err != nil {
		return errors.Wrap(err, "server failed to stop")
	}

	s.server.Close()

	return nil
}

// Create a serverOptions object with sane defaults.
func defaultNodeOptions() *serverOptions {
	return &serverOptions{
		Log: client.DefaultLogFunc(),
	}
}
