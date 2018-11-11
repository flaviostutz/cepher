//This is a hard fork from the great job done by
//http://github.com/yp-engineering/rbd-docker-plugin
package main

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/Sirupsen/logrus"
	"github.com/docker/go-plugins-helpers/volume"
	// "go-plugins-helpers/volume"
)

const VERSION = "1.1.0-beta"

func main() {
	versionFlag := flag.Bool("version", false, "Print version")
	logLevel := flag.String("loglevel", "info", "debug, info, warning, error")
	cephCluster := flag.String("cluster", "", "Ceph cluster") // less likely to run multiple clusters on same hardware
	cephUser := flag.String("user", "admin", "Ceph user")
	defaultCephPool := flag.String("pool", "volumes", "Default Ceph Pool for RBD operations")
	rootMountDir := flag.String("mount", "/mnt/cepher", "Mount directory for volumes on host")
	cephConfigFile := flag.String("config", "/etc/ceph/ceph.conf", "Ceph cluster config") // more likely to have config file pointing to cluster
	canCreateVolumes := flag.Bool("create", false, "Can auto Create RBD Images")
	defaultImageSizeMB := flag.Int("size", 3*1024, "RBD Image size to Create (in MB) (default: 3072=3GB)")
	defaultImageFSType := flag.String("fs", "xfs", "FS type for the created RBD Image (must have mkfs.type)")
	defaultImageFeatures := flag.String("features", "layering,stripping", "Initial RBD Image features for new images")
	enableExclusiveWriteLock := flag.Bool("write-lock", true, "If true, the plugin will allow only one container with write access to mount a ceph image at a time. Multiple read only mounts will be allowed")
	// writeLockTimeoutMillis := flag.Int64("lock-timeout", 10*1000, "If a host with a mounted device stops sending lock refreshs, it will be release to another host to mount the image after this time")
	defaultRemoveAction := flag.String("remove-action", "rename", "Action to be performed when receiving a command to 'remove' a volume. Options are: 'ignore' (won't remove image from Ceph), 'delete' (will delete image from Ceph - irreversible!) or 'rename' (rename image prefixing it by 'zz_')")
	useRBDKernelModule := flag.Bool("kernel-module", false, "If true, will use the Linux Kernel RBD module for mapping Ceph Images to block devices, which has greater performance, but currently supports only features 'layering', 'striping' and 'exclusive-lock'. Else, use rbd-nbd Ceph library (apt-get install rbd-nbd) which supports all Ceph image features available")
	flag.Parse()

	logrus.Infof("useRBDKernelModule=%s", *useRBDKernelModule)
	switch *logLevel {
	case "debug":
		logrus.SetLevel(logrus.DebugLevel)
		break
	case "warning":
		logrus.SetLevel(logrus.WarnLevel)
		break
	case "error":
		logrus.SetLevel(logrus.ErrorLevel)
		break
	default:
		logrus.SetLevel(logrus.InfoLevel)
	}

	if *versionFlag {
		logrus.Infof("%s\n", VERSION)
		return
	}

	if *enableExclusiveWriteLock && !*useRBDKernelModule {
		logrus.Errorf("option write-lock=true cannot be used when kernel-module=true. to use exclusive write lock, use kernel-module=false instead")
		return
	}

	logrus.Infof("====Starting Cepher plugin version %s====", VERSION)

	d := newCephRBDVolumeDriver(
		*cephCluster,
		*cephUser,
		*defaultCephPool,
		*rootMountDir,
		*cephConfigFile,
		*canCreateVolumes,
		*defaultImageSizeMB,
		*defaultImageFSType,
		*defaultImageFeatures,
		*defaultRemoveAction,
		*enableExclusiveWriteLock,
		*writeLockTimeoutMillis,
		*useRBDKernelModule,
	)

	logrus.Debugf("Initializing driver instance")
	err := d.init()
	if err != nil {
		logrus.Errorf("error during driver initialization: %s", err)
	}

	logrus.Debugf("Creating Docker VolumeDriver Handler")
	h := volume.NewHandler(d)

	socketAddress := "/run/docker/plugins/cepher.sock"
	logrus.Infof("Opening Socket for Docker to connect at %s gid=%s", socketAddress, currentGid())
	// ensure directory exists
	err = os.MkdirAll(filepath.Dir(socketAddress), os.ModeDir)
	if err != nil {
		logrus.Errorf("Error creating socket directory: %s", err)
	}

	// open socket
	err = h.ServeUnix(socketAddress, currentGid())
	if err != nil {
		logrus.Errorf("Unable to create UNIX socket: %v", err)
	}
}
