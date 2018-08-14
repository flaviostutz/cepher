//This is a hard fork from the great job done by 
//http://github.com/yp-engineering/rbd-docker-plugin
package main

// Ceph RBD VolumeDriver Docker Plugin, setup config and go

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/docker/go-plugins-helpers/volume"
    // "go-plugins-helpers/volume"
)

var (
	VALID_REMOVE_ACTIONS = []string{"ignore", "delete", "rename"}

	// Plugin Option Flags
	versionFlag        = flag.Bool("version", false, "Print version")
	debugFlag          = flag.Bool("debug", false, "Debug output")
	pluginName         = flag.String("name", "cepher", "Docker plugin name for use on --volume-driver option")
	cephUser           = flag.String("user", "admin", "Ceph user")
	cephConfigFile     = flag.String("config", "/etc/ceph/ceph.conf", "Ceph cluster config") // more likely to have config file pointing to cluster
	cephCluster        = flag.String("cluster", "", "Ceph cluster")                          // less likely to run multiple clusters on same hardware
	defaultCephPool    = flag.String("pool", "volumes", "Default Ceph Pool for RBD operations")
	pluginDir          = flag.String("plugins", "/run/docker/plugins", "Docker plugin directory for socket")
	rootMountDir       = flag.String("mount", volume.DefaultDockerRootDirectory, "Mount directory for volumes on host")
	canCreateVolumes   = flag.Bool("create", false, "Can auto Create RBD Images")
	defaultImageSizeMB = flag.Int("size", 3*1024, "RBD Image size to Create (in MB) (default: 3072=3GB)")
	defaultImageFSType = flag.String("fs", "xfs", "FS type for the created RBD Image (must have mkfs.type)")
)

// setup a validating flag for remove action
type removeAction string

func (a *removeAction) String() string {
	return string(*a)
}

func (a *removeAction) Set(value string) error {
	if !contains(VALID_REMOVE_ACTIONS, value) {
		return errors.New(fmt.Sprintf("Invalid value: %s, valid values are: %q", value, VALID_REMOVE_ACTIONS))
	}
	*a = removeAction(value)
	return nil
}

func contains(vals []string, check string) bool {
	for _, v := range vals {
		if check == v {
			return true
		}
	}
	return false
}

var removeActionFlag removeAction = "ignore"

func init() {
	flag.Var(&removeActionFlag, "remove", "Action to take on Remove: ignore, delete or rename")
	flag.Parse()
}

func socketPath() string {
	return filepath.Join(*pluginDir, *pluginName+".sock")
}

func main() {
	if *versionFlag {
		fmt.Printf("%s\n", VERSION)
		return
	}

	log.Printf("----CEPHER----")
	log.Printf("INFO: starting Cepher plugin version %s", VERSION)
	log.Printf("INFO: canCreateVolumes=%v, removeAction=%q", *canCreateVolumes, removeActionFlag)
	log.Printf(
		"INFO: Setting up Ceph Driver for PluginID=%s, cluster=%s, ceph-user=%s, pool=%s, mount=%s, config=%s",
		*pluginName,
		*cephCluster,
		*cephUser,
		*defaultCephPool,
		*rootMountDir,
		*cephConfigFile,
	)

	// double check for config file - required especially for non-standard configs
	if *cephConfigFile == "" {
		log.Fatal("FATAL: Unable to use ceph rbd tool without config file")
	}
	if _, err := os.Stat(*cephConfigFile); os.IsNotExist(err) {
		log.Fatalf("FATAL: Unable to find ceph config needed for ceph rbd tool: %s", err)
	}

	// build driver struct -- but don't create connection yet
	d := newCephRBDVolumeDriver(
		*pluginName,
		*cephCluster,
		*cephUser,
		*defaultCephPool,
		*rootMountDir,
		*cephConfigFile,
	)

	log.Println("INFO: Initializing driver instance")
	err := d.init()
	if err != nil {
		log.Fatalf("FATAL: error during driver initialization: %s", err)
	}

	log.Println("INFO: Creating Docker VolumeDriver Handler")
	h := volume.NewHandler(d)

	socket := socketPath()
	log.Printf("INFO: Opening Socket for Docker to connect: %s", socket)
	// ensure directory exists
	err = os.MkdirAll(filepath.Dir(socket), os.ModeDir)
	if err != nil {
		log.Fatalf("FATAL: Error creating socket directory: %s", err)
	}

	// setup signal handling after logging setup and creating driver, in order to signal the logfile and ceph connection
	// NOTE: systemd will send SIGTERM followed by SIGKILL after a timeout to stop a service daemon
	signalChannel := make(chan os.Signal, 2) // chan with buffer size 2
	signal.Notify(signalChannel, syscall.SIGTERM, syscall.SIGKILL)
	go func() {
		for sig := range signalChannel {
			//sig := <-signalChannel
			switch sig {
			case syscall.SIGTERM, syscall.SIGKILL:
				log.Printf("INFO: received TERM or KILL signal: %s", sig)
				os.Exit(0)
			}
		}
	}()

	// open socket
	err = h.ServeUnix(socket, currentGid())

	if err != nil {
		log.Printf("ERROR: Unable to create UNIX socket: %v", err)
	}
}

// isDebugEnabled checks for RBD_DOCKER_PLUGIN_DEBUG environment variable
func isDebugEnabled() bool {
	return *debugFlag || os.Getenv("RBD_DOCKER_PLUGIN_DEBUG") == "1"
}
