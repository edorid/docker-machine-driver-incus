package incus

import (
	"fmt"
	"net"
	"os"
	"slices"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type Driver struct {
	*drivers.BaseDriver
	URL               string
	TLSClientCert     string
	TLSClientKey      string
	CPU               int
	Memory            int
	DiskSize          int
	Project           string
	Profile           string
	Network           string
	Storage           string
	Image             string
	CloudInitUserData string
	incus             incus.InstanceServer
	state             state.State
	sshPublicKey      string
	imgConfig         *api.InstanceSource
	netConfig         map[string]string
	diskConfig        map[string]string
	rsrcConfig        map[string]string
}

const (
	driverName           = "incus"
	defaultCpus          = 1
	defaultMemory        = 1024
	defaultDiskSize      = 10240
	defaultProject       = "default"
	defaultProfile       = "default"
	defaultNetwork       = "incusbr0"
	defaultStorage       = "local"
	defaultActiveTimeout = 200
	imageServer          = "https://images.linuxcontainers.org"
	cloudInitVendorData  = `#cloud-config
allow_public_ssh_keys: true
ssh_authorized_keys:
  - %s
no_ssh_fingerprints: false
ssh:
  emit_keys_to_console: false
disable_root: false
package_update: true
packages:
  - openssh-server
  - systemd-timesyncd
  - curl
`
)

func NewDriver(hostName, storePath string) drivers.Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			EnvVar: "INCUS_URL",
			Name:   "incus-url",
			Usage:  "Incus Server URL (ex: https://incus.example.com:8443)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_TLS_CLIENT_CERT",
			Name:   "incus-tls-client-cert",
			Usage:  "TLS client certificate",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_TLS_CLIENT_KEY",
			Name:   "incus-tls-client-key",
			Usage:  "TLS client key",
			Value:  "",
		},
		mcnflag.IntFlag{
			EnvVar: "INCUS_CPU_COUNT",
			Name:   "incus-cpu-count",
			Usage:  "Incus CPU number for VM",
			Value:  defaultCpus,
		},
		mcnflag.IntFlag{
			EnvVar: "INCUS_MEMORY_SIZE",
			Name:   "incus-memory-size",
			Usage:  "Incus size of memory for VM (in MiB)",
			Value:  defaultMemory,
		},
		mcnflag.IntFlag{
			EnvVar: "INCUS_DISK_SIZE",
			Name:   "incus-disk-size",
			Usage:  "Incus size of disk for VM (in MiB)",
			Value:  defaultDiskSize,
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_PROJECT",
			Name:   "incus-project",
			Usage:  "Incus project name",
			Value:  defaultProject,
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_PROFILE",
			Name:   "incus-profile",
			Usage:  "Incus profile name",
			Value:  defaultProfile,
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_NETWORK_NAME",
			Name:   "incus-network-name",
			Usage:  "Incus network name",
			Value:  defaultNetwork,
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_STORAGE_NAME",
			Name:   "incus-storage-name",
			Usage:  "Incus storage name",
			Value:  defaultStorage,
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_IMAGE_NAME",
			Name:   "incus-image-name",
			Usage:  "Incus image name (alias)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "INCUS_CLOUDINIT_USERDATA",
			Name:   "incus-cloudinit-userdata",
			Usage:  "Incus cloud-init.user-data",
			Value:  "",
		},
	}
}

func (d *Driver) Create() error {
	log.Infof("Creating Incus instance...")

	pubKey, err := d.getSSHKey()
	if err != nil {
		return err
	}
	d.sshPublicKey = pubKey

	client, err := d.getClient()
	if err != nil {
		return err
	}

	cloudInitVendorData := fmt.Sprintf(cloudInitVendorData, d.sshPublicKey)
	config := d.rsrcConfig
	config["cloud-init.vendor-data"] = cloudInitVendorData
	if d.CloudInitUserData != "" {
		if cloudConfig, err := os.ReadFile(d.CloudInitUserData); err == nil {
			config["cloud-init.user-data"] = string(cloudConfig)
		}
	}

	devices := map[string]map[string]string{
		"root": d.diskConfig,
		"eth0": d.netConfig,
	}

	instance := api.InstancePut{
		Profiles:    []string{d.Profile},
		Description: "Created by Rancher Machine",
		Config:      config,
		Devices:     devices,
	}

	req := api.InstancesPost{
		Name:        d.MachineName,
		Type:        api.InstanceTypeVM,
		Start:       true,
		Source:      *d.imgConfig,
		InstancePut: instance,
	}

	op, err := client.CreateInstance(req)
	if err != nil {
		return err
	}

	err = op.Wait()
	if err != nil {
		return err
	}

	const maxRetries = 100
	retry := 0
	for {
		state, _, err := client.GetInstanceState(d.MachineName)
		if err != nil {
			return err
		}

		if slices.Contains([]api.StatusCode{api.Aborting, api.Freezing, api.Frozen, api.Thawed, api.Error, api.Failure, api.Cancelled}, state.StatusCode) {
			return fmt.Errorf("instance state is %s", state.StatusCode)
		}

		for _, net := range state.Network {
			// only take the first IPv4 address
			for _, addr := range net.Addresses {
				if addr.Family == "inet" {
					d.IPAddress = addr.Address
					log.Infof("Instance IP address: %s", d.IPAddress)
					return nil
				}
			}
		}

		time.Sleep(5 * time.Second)
		retry += 1
		if retry > maxRetries {
			return fmt.Errorf("timeout waiting for instance to get IP address")
		}
	}
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return driverName
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetURL() (string, error) {
	if err := drivers.MustBeRunning(d); err != nil {
		return "", err
	}

	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, "2376")), nil
}

func (d *Driver) GetState() (state.State, error) {
	client, err := d.getClient()
	if err != nil {
		return state.Error, err
	}

	instance, _, err := client.GetInstanceState(d.MachineName)
	if err != nil {
		return state.Error, err
	}
	switch instance.StatusCode.String() {
	case "Starting":
		return state.Starting, nil
	case "Running":
		return state.Running, nil
	case "Stopping":
		return state.Starting, nil
	case "Stopped":
		return state.Stopped, nil
	case "Frozen":
		return state.Paused, nil
	}

	return state.None, nil
}

func (d *Driver) Kill() error {
	client, err := d.getClient()
	if err != nil {
		return err
	}

	state := api.InstanceStatePut{
		Action: "stop",
		Force:  true,
	}

	op, err := client.UpdateInstanceState(d.MachineName, state, "")
	if err != nil {
		return err
	}

	err = op.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (d *Driver) PreCreateCheck() error {
	log.Infof("Running pre-create checks...")

	client, err := d.getClient()
	if err != nil {
		return err
	}

	if _, _, err := client.GetProfile(d.Profile); err != nil {
		return fmt.Errorf("profile %s not found: %w", d.Profile, err)
	}

	d.imgConfig, err = d.getImage()
	if err != nil {
		return err
	}

	d.netConfig, err = d.getNetwork()
	if err != nil {
		return err
	}

	d.diskConfig, err = d.getStorage()
	if err != nil {
		return err
	}

	d.rsrcConfig, err = d.getResource()
	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) Remove() error {
	err := d.Kill()
	if err != nil {
		return err
	}

	client, err := d.getClient()
	if err != nil {
		return err
	}

	op, err := client.DeleteInstance(d.MachineName)
	if err != nil {
		return err
	}

	err = op.Wait()
	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) Restart() error {
	client, err := d.getClient()
	if err != nil {
		return err
	}

	state := api.InstanceStatePut{
		Action: "restart",
	}

	op, err := client.UpdateInstanceState(d.MachineName, state, "")
	if err != nil {
		return err
	}

	err = op.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.URL = flags.String("incus-url")
	d.TLSClientCert = flags.String("incus-tls-client-cert")
	d.TLSClientKey = flags.String("incus-tls-client-key")
	d.CPU = flags.Int("incus-cpu-count")
	d.Memory = flags.Int("incus-memory-size")
	d.DiskSize = flags.Int("incus-disk-size")
	d.Project = flags.String("incus-project")
	d.Profile = flags.String("incus-profile")
	d.Network = flags.String("incus-network-name")
	d.Storage = flags.String("incus-storage-name")
	d.Image = flags.String("incus-image-name")
	d.CloudInitUserData = flags.String("incus-cloudinit-userdata")

	d.SetSwarmConfigFromFlags(flags)

	return nil
}

func (d *Driver) Start() error {
	client, err := d.getClient()
	if err != nil {
		return err
	}

	state := api.InstanceStatePut{
		Action: "start",
	}

	op, err := client.UpdateInstanceState(d.MachineName, state, "")
	if err != nil {
		return err
	}

	err = op.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (d *Driver) Stop() error {
	client, err := d.getClient()
	if err != nil {
		return err
	}

	state := api.InstanceStatePut{
		Action: "stop",
		Force:  false,
	}

	op, err := client.UpdateInstanceState(d.MachineName, state, "")
	if err != nil {
		return err
	}

	err = op.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (d *Driver) Upgrade() error {
	return fmt.Errorf("upgrade is not supported for incus driver at this moment")
}

func (d *Driver) getClient() (incus.InstanceServer, error) {
	if d.incus != nil {
		return d.incus, nil
	}

	args := &incus.ConnectionArgs{
		TLSClientCert:      d.TLSClientCert,
		TLSClientKey:       d.TLSClientKey,
		InsecureSkipVerify: true,
	}

	is, err := incus.ConnectIncus(d.URL, args)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to incus: " + err.Error())
	}

	if _, _, err := is.GetProject(d.Project); err != nil {
		return nil, fmt.Errorf("project %s not found: %w", d.Project, err)
	}

	d.incus = is.UseProject(d.Project)
	return d.incus, nil
}

func (d *Driver) publicSSHKeyPath() string {
	return d.GetSSHKeyPath() + ".pub"
}

func (d *Driver) getSSHKey() (string, error) {
	log.Infof("Generating SSH key on %s...", d.GetSSHKeyPath())
	if err := ssh.GenerateSSHKey(d.GetSSHKeyPath()); err != nil {
		return "", err
	}
	pubKey, err := os.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return "", err
	}

	return string(pubKey), nil
}

func (d *Driver) getImage() (*api.InstanceSource, error) {
	if d.Image == "" {
		return nil, fmt.Errorf("image is required")
	}

	client, err := d.getClient()
	if err != nil {
		return nil, err
	}

	// check if image name is from local image
	if _, _, err := client.GetImageAlias(d.Image); err == nil {
		return &api.InstanceSource{
			Type:  "image",
			Alias: d.Image,
		}, nil
	}

	imgSrv, err := incus.ConnectSimpleStreams(imageServer, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to image server: %w", err)
	}

	if _, _, err := imgSrv.GetImageAlias(d.Image); err != nil {
		return nil, fmt.Errorf("image %s not found in image server", d.Image)
	}

	// image is from remote image server
	return &api.InstanceSource{
		Type:     "image",
		Alias:    d.Image,
		Server:   imageServer,
		Protocol: "simplestreams",
	}, nil
}

func (d *Driver) getNetwork() (map[string]string, error) {
	if d.Network == "" {
		return nil, fmt.Errorf("network is required")
	}

	client, err := d.getClient()
	if err != nil {
		return nil, err
	}

	network, _, err := client.GetNetwork(d.Network)
	if err != nil {
		return nil, fmt.Errorf("network %s not found: %w", d.Network, err)
	}

	if !slices.Contains([]string{"bridge", "ovn"}, network.Type) {
		return nil, fmt.Errorf("network type %s not supported", network.Type)
	}

	// bridge
	if network.Type == "bridge" {
		return map[string]string{
			"name":    d.Network,
			"type":    "nic",
			"nictype": "bridged",
			"parent":  d.Network,
		}, nil
	}

	// ovn network
	return map[string]string{
		"name":    "eth0",
		"type":    "nic",
		"network": d.Network,
	}, nil
}

func (d *Driver) getStorage() (map[string]string, error) {
	if d.Storage == "" {
		return nil, fmt.Errorf("storage is required")
	}

	client, err := d.getClient()
	if err != nil {
		return nil, err
	}

	_, _, err = client.GetStoragePool(d.Storage)
	if err != nil {
		return nil, fmt.Errorf("storage %s not found: %w", d.Storage, err)
	}

	return map[string]string{
		"type": "disk",
		"path": "/",
		"pool": d.Storage,
		"size": fmt.Sprintf("%dMiB", d.DiskSize),
	}, nil
}

func (d *Driver) getResource() (map[string]string, error) {
	return map[string]string{
		"limits.cpu":    fmt.Sprintf("%d", d.CPU),
		"limits.memory": fmt.Sprintf("%dMiB", d.Memory),
	}, nil
}
