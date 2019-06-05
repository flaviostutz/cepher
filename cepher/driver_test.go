package main

import (
	"flag"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/docker/go-plugins-helpers/volume"
	"github.com/sirupsen/logrus"
)

var messages chan int
var wg sync.WaitGroup

func TestListCommand(t *testing.T) {
	logrus.Infof("====Starting Cepher TEST plugin version ====")

	//Environment Variables
	lockEtcdAddress := os.Getenv("ETCD_URL")
	logrus.Infof("LockEtcdAddress: %s", lockEtcdAddress)

	logLevel := flag.String("loglevel", "debug", "debug, info, warning, error")
	cephCluster := flag.String("cluster", "", "Ceph cluster") // less likely to run multiple clusters on same hardware
	cephUser := flag.String("user", "admin", "Ceph user")
	defaultCephPool := flag.String("pool", "volumes", "Default Ceph Pool for RBD operations")
	rootMountDir := flag.String("mount", "/mnt/cepher", "Mount directory for volumes on host")
	cephConfigFile := flag.String("config", "/etc/ceph/ceph.conf", "Ceph cluster config") // more likely to have config file pointing to cluster
	canCreateVolumes := flag.Bool("create", true, "Can auto Create RBD Images")
	defaultImageSizeMB := flag.Int("size", 1*100, "RBD Image size to Create (in MB)")
	defaultImageFSType := flag.String("fs", "xfs", "FS type for the created RBD Image (must have mkfs.type)")
	defaultImageFeatures := flag.String("features", "layering,striping,exclusive-lock,object-map,fast-diff,journaling", "Initial RBD Image features for new images")
	defaultRemoveAction := flag.String("remove-action", "delete", "Action to be performed when receiving a command to 'remove' a volume. Options are: 'ignore' (won't remove image from Ceph), 'delete' (will delete image from Ceph - irreversible!) or 'rename' (rename image prefixing it by 'zz_')")
	useRBDKernelModule := flag.Bool("kernel-module", false, "If true, will use the Linux Kernel RBD module for mapping Ceph Images to block devices, which has greater performance, but currently supports only features 'layering', 'striping' and 'exclusive-lock'. Else, use rbd-nbd Ceph library (apt-get install rbd-nbd) which supports all Ceph image features available")
	lockEtcdServers := flag.String("lock-etcd", lockEtcdAddress, "ETCD server addresses used for distributed lock management. ex.: 192.168.1.1:2379,192.168.1.2:2379")
	lockTimeoutMillis := flag.Uint64("lock-timeout", 10*1000, "If a host with a mounted device stops sending lock refreshs, it will be release to another host to mount the image after this time")

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

	driver := cephRBDVolumeDriver{
		cephCluster:          *cephCluster,
		cephUser:             *cephUser,
		defaultCephPool:      *defaultCephPool,
		rootMountDir:         *rootMountDir,
		cephConfigFile:       *cephConfigFile,
		canCreateVolumes:     *canCreateVolumes,
		defaultImageSizeMB:   *defaultImageSizeMB,
		defaultImageFSType:   *defaultImageFSType,
		defaultImageFeatures: *defaultImageFeatures,
		defaultRemoveAction:  *defaultRemoveAction,
		useRBDKernelModule:   *useRBDKernelModule,
		lockEtcdServers:      *lockEtcdServers,
		lockTimeoutMillis:    *lockTimeoutMillis,
		m:                    &sync.Mutex{},
	}

	logrus.Debugf("Initializing driver instance")
	err := driver.init()
	logrus.Debugf("etcdLockSession=%v", driver.etcdLockSession)
	logrus.Debugf("deviceLocks=%v", driver.deviceLocks)
	if err != nil {
		logrus.Errorf("error during driver initialization: %s", err)
	}

	// List Current Volumes
	logrus.Debugf("Listing current images at Ceph")
	response, err := driver.List()
	if err != nil {
		logrus.Debugf("Error at Listing current images")
		panic("Error at Listing current images")
	}
	logrus.Debug(*response)

	// Start 6 cicles of Create/Mount/Unmount/Remove Images from Ceph
	wg.Add(6)
	go DoCompleteTask("volumes/test-1", driver)
	go DoCompleteTask("volumes/test-2", driver)
	go DoCompleteTask("volumes/test-3", driver)
	go DoCompleteTask("volumes/test-4", driver)
	go DoCompleteTask("volumes/test-5", driver)
	go DoCompleteTask("volumes/test-6", driver)

	wg.Wait()
	logrus.Infof("==== Done! ====")
}

func DoCompleteTask(imageName string, driver cephRBDVolumeDriver) {
	defer wg.Done()
	logrus.Debugf("Inciado %s", imageName)

	//# Create Requests to Call at the same format received from docker volumes interface
	var reqCreate volume.CreateRequest
	reqCreate.Name = imageName
	var reqMount volume.MountRequest
	reqMount.Name = imageName
	var reqUmount volume.UnmountRequest
	reqUmount.Name = imageName
	var reqRemove volume.RemoveRequest
	reqRemove.Name = imageName

	err := driver.Create(&reqCreate)
	if err != nil {
		logrus.Debugf("Error at Create Image")
		// panic("Erro at Create Image")
	}
	logrus.Debugf("Image created %s", imageName)

	response, err := driver.Mount(&reqMount)
	if err != nil {
		logrus.Debugf("Erro at Mount Image")
		panic("Erro at mount image")
	}
	logrus.Debugf("Image mounted Name: %s %s", imageName, response)

	time.Sleep(10 * time.Second)

	logrus.Debugf("------- LIST MAPPED DEVICES ---------")
	volumes, err := driver.listMappedDevices()
	for _, item := range volumes {
		logrus.Debugf("--> Volume %q", item)
	}

	time.Sleep(10 * time.Second)

	err = driver.Unmount(&reqUmount)
	if err != nil {
		logrus.Debugf("Erro at Umount Image")
		panic("Erro at umount image")
	}
	logrus.Debugf("Image umounted %s", imageName)

	err = driver.Remove(&reqRemove)
	if err != nil {
		logrus.Debugf("Erro at Remove Image")
		panic("Erro at Remove image")
	}
	logrus.Debugf("Image removed %s", imageName)

	logrus.Debugf("Done with %s", imageName)
	return
}
