// Package deviceplugin provides a basic implementation of a Kubernetes DevicePlugin without content.
// It is intended to be used to build other DevicePlugins.
package deviceplugin

import (
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
	"log"
	"net"
	"os"
	"path"
	"time"
)

type DevicePlugin struct {
	socket       string
	resourceName string
	server       *grpc.Server
}

func NewDevicePlugin(serversock string, resourcename string) *DevicePlugin {
	return &DevicePlugin{
		socket:       serversock,
		resourceName: resourcename,
	}
}

func dial(ctx context.Context, unixSocketPath string) (*grpc.ClientConn, error) {
	c, err := grpc.DialContext(ctx, unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}
	return c, nil
}

func (d *DevicePlugin) cleanup() error {
	_, serr := os.Stat(d.socket)
	if serr != nil && os.IsNotExist(serr) {
		return nil
	}

	if err := os.Remove(d.socket); err != nil {
		return err
	}

	return nil
}

func (d *DevicePlugin) Start() error {
	err := d.cleanup()

	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", d.socket)
	if err != nil {
		return err
	}
	d.server = grpc.NewServer([]grpc.ServerOption{}...)
	pluginapi.RegisterDevicePluginServer(d.server, d)

	go d.server.Serve(sock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := dial(ctx, d.socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer cancel()

	return nil
}

func (d *DevicePlugin) Stop() error {
	if d.server == nil {
		return nil
	}

	d.server.Stop()
	d.server = nil
	return d.cleanup()
}

func (d *DevicePlugin) Serve() error {
	err := d.Start()
	if err != nil {
		log.Printf("Could not start device plugin %s", err)
	}
	log.Println("Starting to serve on", d.socket)

	err = d.Register(pluginapi.KubeletSocket, d.resourceName)
	if err != nil {
		log.Printf("Could not register device plugin %s", err)
		return err
	}
	log.Println("Registered device plugin with Kubelet")
	return nil
}

// Define functions needed to meet the Kubernetes DevicePlugin API

func (d *DevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

func (d *DevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	return nil, nil
}

func (d *DevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	return nil
}

func (d *DevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (d *DevicePlugin) Register(kubeletEndpoint, resourceName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := dial(ctx, kubeletEndpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer cancel()
	client := pluginapi.NewRegistrationClient(conn)
	request := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(d.socket),
		ResourceName: resourceName,
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.Register(ctx, request)
	defer cancel()
	if err != nil {
		return err
	}
	return nil
}
