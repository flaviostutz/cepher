//This is a hard fork from the great job done by
//http://github.com/yp-engineering/rbd-docker-plugin
package main

/**
 * Ceph RBD Docker VolumeDriver Plugin
 *
 * rbd-docker-plugin service creates a UNIX socket that can accept Volume
 * Driver requests (JSON HTTP POSTs) from Docker Engine.
 *
 * Historical note: Due to some issues using the go-ceph library for
 * locking/unlocking, we reimplemented all functionality to use shell CLI
 * commands via the 'rbd' executable.
 *
 * System Requirements:
 *   - requires rbd CLI binary in PATH
 *
 * Plugin name: rbd  -- configurable via --name
 *
 * % docker run --volume-driver=rbd -v imagename:/mnt/dir IMAGE [CMD]
 *
 * golang github code examples:
 * - https://github.com/docker/docker/blob/master/experimental/plugins_volume.md
 * - https://github.com/docker/go-plugins-helpers/tree/master/volume
 * - https://github.com/calavera/docker-volume-glusterfs
 * - https://github.com/AcalephStorage/docker-volume-ceph-rbd
 */

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/docker/go-plugins-helpers/volume"
	"github.com/flaviostutz/etcd-lock/etcdlock"
	"github.com/sirupsen/logrus"
)

var (
	rbdUnmapBusyRegexp         = regexp.MustCompile(`^exit status 16$`)
	spaceDelimitedFieldsRegexp = regexp.MustCompile(`([^\s]+)`)
)

// Volume is our local struct to store info about RBD Image
type Volume struct {
	Pool      string
	Name      string // RBD Image name
	Device    string // local host kernel device (e.g. /dev/rbd1)
	Mountpath string
}

// our driver type for impl func
type cephRBDVolumeDriver struct {
	cephCluster          string
	cephUser             string
	defaultCephPool      string
	rootMountDir         string
	cephConfigFile       string
	canCreateVolumes     bool
	canCreatePools       bool
	defaultImageSizeMB   int
	defaultImageFSType   string
	defaultImageFeatures string
	defaultRemoveAction  string
	defaultPoolPgNum     string
	useRBDKernelModule   bool
	lockEtcdServers      string
	lockTimeoutMillis    uint64
	m                    *sync.Mutex
	etcdLockSession      *concurrency.Session
	volumeMountLocks     map[string]map[string]*etcdlock.RWMutex
}

func (d *cephRBDVolumeDriver) init() error {
	if d.useRBDKernelModule {
		logrus.Warn("The driver is configured to use the RBD Kernel Module. It has better performance but currently supports only image features layering, stripping and exclusive-lock")
	}

	//TODO reconstruct locks from real kernel mapped devices on driver restart
	if d.lockEtcdServers != "" {
		logrus.Debugf("Setting up ETCD client to %s", d.lockEtcdServers)
		endpoints := strings.Split(d.lockEtcdServers, ",")
		cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints})
		if err != nil {
			return err
		}
		logrus.Debugf("ETCD client initiated")

		logrus.Debugf("Creating ETCD Lock Session")
		d.etcdLockSession, err = concurrency.NewSession(cli, concurrency.WithTTL(int(d.lockTimeoutMillis/1000)))
		if err != nil {
			return err
		}
		logrus.Debugf("ETCD lock session ok %v", d.etcdLockSession)
		d.volumeMountLocks = make(map[string]map[string]*etcdlock.RWMutex)
	}

	logrus.Debugf("Driver initialized")
	return nil
}

// ************************************************************
//
// Implement the Docker Volume Driver interface
//
// Using https://github.com/docker/go-plugins-helpers/
//
// ************************************************************

// Capabilities
// Scope: global - images managed using rbd plugin can be considered "global"
func (d cephRBDVolumeDriver) Capabilities() *volume.CapabilitiesResponse {
	return &volume.CapabilitiesResponse{
		Capabilities: volume.Capability{
			Scope: "global",
		},
	}
}

// Create will ensure the RBD image requested is available.  Plugin requires
// --create option flag to be able to provision new RBD images.
//
// Docker Volume Create Options:
//   size   - in MB
//   pool
//   fstype
//
//
// POST /VolumeDriver.Create
//
// Request:
//    {
//      "Name": "volume_name",
//      "Opts": {}
//    }
//    Instruct the plugin that the user wants to create a volume, given a user
//    specified volume name. The plugin does not need to actually manifest the
//    volume on the filesystem yet (until Mount is called).
//
// Response:
//    { "Err": null }
//    Respond with a string error if an error occurred.
//
func (d cephRBDVolumeDriver) Create(r *volume.CreateRequest) error {
	d.m.Lock()
	defer d.m.Unlock()
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API CREATE(%q)", r)
	return d.CreateInternal(r)
}

func (d cephRBDVolumeDriver) CreateInternal(r *volume.CreateRequest) error {
	logrus.Debugf("CreateInternal(%q)", r)
	// parse image name optional/default pieces
	pool, name, _, err := d.parseImagePoolName(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	mutex, err := d.lockCreateVolume(pool, name)
	if err != nil {
		logrus.Errorf("error locking volume %s for create: %s", r.Name, err.Error())
		return err
	}
	defer func() {
		if err := d.unlockCreateVolume(mutex); err != nil {
			logrus.Errorf("error unlocking volume %s for create: %s", r.Name, err.Error())
		}
	}()

	fstype := d.defaultImageFSType
	imageFeatures := d.defaultImageFeatures

	// Options to override from `docker volume create -o OPT=VAL ...`
	if r.Options["pool"] != "" {
		pool = r.Options["pool"]
	}
	if r.Options["name"] != "" {
		name = r.Options["name"]
	}

	size := d.defaultImageSizeMB

	if r.Options["size"] != "" {
		size, err = strconv.Atoi(r.Options["size"])
		if err != nil {
			err := fmt.Sprintf("unable to parse int from %s: %s", r.Options["size"], err)
			logrus.Errorf("%s", err)
			return errors.New(err)
		}
	}
	if r.Options["fstype"] != "" {
		fstype = r.Options["fstype"]
	}
	if r.Options["features"] != "" {
		imageFeatures = r.Options["features"]
	}

	// verify if pool exists
	poolExists, err := poolExists(pool)
	if err != nil {
		err := fmt.Sprintf("error while checking if pool '%s' exists: %s", pool, err)
		logrus.Error(err)
		return errors.New(err)
	}
	if !poolExists {
		err := d.createPool(pool)
		if err != nil {
			return err
		}
	}

	logrus.Debug("verify if image already exists on RBD cluster")
	exists, err := d.rbdImageExists(pool, name)
	if err != nil {
		err := fmt.Sprintf("error while checking RBD Image %s/%s: %s", pool, name, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}
	if !exists {
		logrus.Debugf("Ceph Image doesn't exist yet")
		if d.canCreateVolumes {
			logrus.Debugf("create image on RBD Cluster")
			err = d.createRBDImage(pool, name, size, fstype, imageFeatures)
			if err != nil {
				errString := fmt.Sprintf("Unable to create RBD Image %s/%s: %s", pool, name, err)
				logrus.Errorf(errString)
				return errors.New(errString)
			} else {
				logrus.Infof("New RBD Image %s/%s created successfully", pool, name)
			}
		} else {
			errString := fmt.Sprintf("RBD Image %s/%s not found and the plugin is not enabled for automatic image creation", pool, name)
			logrus.Warnf(errString)
			return errors.New(errString)
		}
	} else {
		logrus.Infof("Image %s/%s already exists in RBD cluster. Reusing it.", pool, name)
	}

	// _, err1 := d.MountInternal(&volume.MountRequest{Name: fmt.Sprintf("%s/%s", pool, name)})
	// if err1 != nil {
	// 	errString := fmt.Sprintf("Error mounting image %s/%s: %s", pool, name, err1)
	// 	logrus.Errorf(errString)
	// 	return errors.New(errString)
	// }

	return nil
}

// POST /VolumeDriver.Remove
//
// Request:
//    { "Name": "volume_name" }
//    Remove a volume, given a user specified volume name.
//
// Response:
//    { "Err": null }
//    Respond with a string error if an error occurred.
//
func (d cephRBDVolumeDriver) Remove(r *volume.RemoveRequest) error {
	d.m.Lock()
	defer d.m.Unlock()
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API REMOVE(%q)", r)
	return d.RemoveInternal(r)
}

func (d cephRBDVolumeDriver) RemoveInternal(r *volume.RemoveRequest) error {
	logrus.Debugf("API RemoveInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, _, err := d.parseImagePoolName(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	logrus.Debugf("verify if RBD Image exists in cluster")
	exists, err := d.rbdImageExists(pool, name)
	if err != nil {
		err := fmt.Sprintf("error checking for RBD Image %s/%s: %s", pool, name, err)
		logrus.Errorf("%s", err)
		return errors.New(err)

	} else if !exists {
		errString := fmt.Sprintf("RBD Image %s/%s not found", pool, name)
		logrus.Errorf(errString)
		return errors.New(errString)
	} else {
		logrus.Debugf("RBD Image %s/%s exists. Proceeding to removal using action '%s'", pool, name, d.defaultRemoveAction)
	}

	// // attempt to gain lock before remove - lock seems to disappear after rm (but not after rename)
	// locker, err := d.lockImage(pool, name)
	// if err != nil {
	// 	errString := fmt.Sprintf("Unable to lock image for remove: %s", name)
	// 	logrus.Errorf(errString)
	// 	return errors.New(errString)
	// }

	// remove action can be: ignore, delete or rename
	if d.defaultRemoveAction == "delete" {
		logrus.Debugf("Deleting RBD Image %s/%s from Ceph Cluster", pool, name)
		err = d.removeRBDImage(pool, name)
		if err != nil {
			errString := fmt.Sprintf("Unable to remove RBD Image %s/%s: %s", pool, name, err)
			logrus.Errorf(errString)
			// defer d.unlockImage(pool, name, locker)
			return errors.New(errString)
		}

		// defer d.unlockImage(pool, name, locker)
	} else if d.defaultRemoveAction == "rename" {
		images, err := d.rbdPoolImageList(pool)
		if err != nil {
			msg := fmt.Sprintf("error getting volume image list from pool %s: %s", pool, err)
			logrus.Error(msg)
			return errors.New(msg)
		}
		backupName, err := generateImageBackupName(name, images)
		if err != nil {
			msg := fmt.Sprintf("error generating image backup name to %s: %s", name, err)
			logrus.Error(msg)
			return errors.New(msg)
		}

		logrus.Debugf("Renaming RBD Image %s/%s in Ceph Cluster to %s/%s", pool, name, pool, backupName)
		err = d.renameRBDImage(pool, name, backupName)
		if err != nil {
			errString := fmt.Sprintf("Unable to rename RBD Image %s/%s to %s/%s: %s", pool, name, pool, backupName, err)
			logrus.Errorf(errString)
			// unlock by old name
			// defer d.unlockImage(pool, name, locker)
			return errors.New(errString)
		} else {
			logrus.Infof("RBD Image %s/%s renamed successfully to %s/%s", pool, name, pool, backupName)
		}
		// unlock by new name
		// defer d.unlockImage(pool, backupName, locker)
		// } else {
		// ignore the remove call - but unlock ?
		// defer d.unlockImage(pool, name, locker)
	} else {
		logrus.Infof("Volume removal requested, but RBD Image %s/%s won't be really deleted.", pool, name)
	}

	// logrus.Debugf("delete local volume reference")
	// delete(d.volumes, mount)
	return nil
}

// Mount will Ceph Map the RBD image to the local kernel and create a mount
// point and mount the image.
//
// POST /VolumeDriver.Mount
//
// Request:
//    { "Name": "volume_name" }
//    Docker requires the plugin to provide a volume, given a user specified
//    volume name. This is called once per container start.
//
// Response:
//    { "Mountpoint": "/path/to/directory/on/host", "Err": null }
//    Respond with the path on the host filesystem where the volume has been
//    made available, and/or a string error if an error occurred.
//
// TODO: utilize the new MountRequest.ID field to track volumes
func (d cephRBDVolumeDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	d.m.Lock()
	defer d.m.Unlock()
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API MOUNT(%q)", r)
	return d.MountInternal(r)
}

func (d cephRBDVolumeDriver) MountInternal(r *volume.MountRequest) (mr *volume.MountResponse, err error) {
	logrus.Debugf("API MountInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, opts, err := d.parseImagePoolName(r.Name)
	readonly := opts == "ro"
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}

	// try to get lock for the volume
	if err := d.lockMountVolume(pool, name, readonly, r.ID); err != nil {
		return nil, err
	}
	defer func() { //Use named return values to perform unlock if error occurred
		if err != nil {
			err = d.unlockMountVolume(pool, name, r.ID)
		}
	}()

	volumes, err := d.currentVolumes()
	if err != nil {
		logrus.Errorf("Error retrieving currently mounted volumes: %s", err)
		return nil, err
	}
	mountpath := d.mountpoint(pool, name, readonly)
	if _, found := volumes[mountpath]; found { //volume already mounted
		// err := fmt.Sprintf("")
		// logrus.Errorf("%s", err)
		// return nil, errors.New(err)
		logrus.Infof("Mountpoint %s already exists. Reusing it. pool=%s image=%s", mountpath, pool, name)
	} else { //volume not mounted yet. mount!
		logrus.Infof("Mountpoint %s doesn't exist yet. Creating it. pool=%s image=%s", mountpath, pool, name)

		// map
		logrus.Debugf("mapping kernel device to RBD Image name=%v, readonly=%v", r.Name, readonly)
		device, err := d.mapImageToDevice(pool, name, readonly)
		if err != nil {
			logrus.Errorf("error mapping RBD Image %s/%s to kernel device: %s", pool, name, err)
			// failsafe: need to release lock
			// defer d.unlockImage(pool, name, locker)
			return nil, errors.New(fmt.Sprintf("Unable to map kernel device. err=%s", err))
		}

		// determine device FS type
		fstype, err := d.deviceType(device)
		if err != nil {
			// logrus.Warnf("unable to detect RBD Image %s/%s fstype: %s", name, err)
			logrus.Warnf("unable to detect RBD Image %s fstype: %s", name, err)
			// NOTE: don't fail - FOR NOW we will assume default plugin fstype
			fstype = d.defaultImageFSType
		}

		// double check image filesystem if possible
		err = d.checkDeviceFilesystem(device, mountpath, fstype, readonly)
		if err != nil {
			logrus.Errorf("Filesystem at RBD Image %s/%s may need repairs: %s", pool, name, err)
			// failsafe: need to release lock and unmap kernel device
			logrus.Debugf("unmapping device")
			defer d.unmapImageDevice(device)
			// defer d.unlockImage(pool, name, locker)
			return nil, errors.New(fmt.Sprintf("Image filesystem has errors. Mount it in a separate machine and perform manual repairs. err=%s", err))
		}

		// check for mountdir - create if necessary
		err = os.MkdirAll(mountpath, os.ModeDir|os.FileMode(int(0775)))
		if err != nil {
			logrus.Errorf("error creating mount directory %s: %s", mountpath, err)
			// failsafe: need to release lock and unmap kernel device
			logrus.Debugf("unmapping device")
			defer d.unmapImageDevice(device)
			// defer d.unlockImage(pool, name, locker)
			return nil, errors.New(fmt.Sprintf("Unable to create mountdir %s", mountpath))
		}

		// mount
		logrus.Debugf("Mounting RBD Image %s/%s, mapped to device %s, to mountdir %s", pool, name, device, mountpath)
		err = d.mountDeviceToPath(fstype, device, mountpath, readonly)
		if err != nil {
			logrus.Errorf("error mounting device %s to directory %s: %s", device, mountpath, err)
			logrus.Debugf("unmapping device")
			defer d.unmapImageDevice(device)
			// defer d.unlockImage(pool, name, locker)
			return nil, errors.New(fmt.Sprintf("Unable to mount device. err=%s", err))
		} else {
			logrus.Infof("Mount to %s successful", mountpath)
		}

	}

	// // attempt to lock
	// locker, err := d.lockImage(pool, name)
	// if err != nil {
	// 	logrus.Errorf("locking RBD Image(%s): %s", name, err)
	// 	return nil, errors.New("Unable to get Exclusive Lock")
	// }

	// map and mount the RBD image -- these are OS level commands, not avail in go-ceph

	return &volume.MountResponse{Mountpoint: mountpath}, nil
}

func (d cephRBDVolumeDriver) lockCreateVolume(pool, name string) (*etcdlock.RWMutex, error) {
	if d.etcdLockSession != nil {
		volumeName := fmt.Sprintf("%s/%s", pool, name)
		mutex := etcdlock.NewRWMutex(d.etcdLockSession, fmt.Sprintf("/cepher-create/%s", volumeName))
		ctx, _ := context.WithTimeout(context.Background(), time.Duration(d.lockTimeoutMillis)*time.Millisecond)
		if err := mutex.RWLock(ctx); err != nil { // using RWLock to allow only one lock at a time
			return nil, err
		}
		logrus.Infof("got RWLock for create volume %s", name)
		return mutex, nil
	}
	return nil, nil
}

func (d cephRBDVolumeDriver) unlockCreateVolume(mutex *etcdlock.RWMutex) error {
	if d.etcdLockSession != nil {
		if err := mutex.Unlock(); err != nil {
			return err
		}
		logrus.Infof("released RWLock for create volume")
	}
	return nil
}

func (d cephRBDVolumeDriver) lockMountVolume(pool, name string, readonly bool, callerID string) error {
	if d.etcdLockSession != nil {
		if callerID == "" {
			return errors.New(fmt.Sprintf("error getting mount lock for volume %s/%s. callerID cannot be an empty string.", pool, name))
		}

		volumeName := fmt.Sprintf("%s/%s", pool, name)
		mutex := etcdlock.NewRWMutex(d.etcdLockSession, fmt.Sprintf("/cepher-mount/%s", volumeName))
		ctx, _ := context.WithTimeout(context.Background(), time.Duration(d.lockTimeoutMillis)*time.Millisecond)
		if readonly {
			if err := mutex.RLock(ctx); err != nil {
				logrus.Debugf("error getting mount read lock for volume %s and caller ID %s", volumeName, callerID)
				return err
			}
			logrus.Infof("got RLock for mount %s", name)
		} else {
			if err := mutex.RWLock(ctx); err != nil {
				logrus.Debugf("error getting mount write lock for volume %s and caller ID %s: %s", volumeName, callerID, err.Error())
				return err
			}
			logrus.Infof("got RWLock for mount %s", name)
		}
		//keep reference with callerID to unlock on unmount volume
		if mutexes, found := d.volumeMountLocks[volumeName]; found {
			mutexes[callerID] = mutex
		} else {
			mutexes := make(map[string]*etcdlock.RWMutex)
			mutexes[callerID] = mutex
			d.volumeMountLocks[volumeName] = mutexes
		}
	}
	return nil
}

func (d cephRBDVolumeDriver) unlockMountVolume(pool, name string, callerID string) error {
	if d.etcdLockSession != nil {
		if callerID == "" {
			return errors.New(fmt.Sprintf("error releasing mount lock for volume %s/%s. callerID cannot be an empty string.", pool, name))
		}

		volumeName := fmt.Sprintf("%s/%s", pool, name)
		mutexes, found := d.volumeMountLocks[volumeName]
		if !found {
			return errors.New(fmt.Sprintf("cannot find locks for volume %s and caller ID %s", volumeName, callerID))
		}

		if mutex, found := mutexes[callerID]; found {
			logrus.Debugf("unlocking volume %s for caller ID %s", volumeName, callerID)
			if err := mutex.Unlock(); err != nil {
				logrus.Errorf("error unlocking volume %s for caller ID %s: %s", volumeName, callerID, err.Error())
				return err
			}
			delete(mutexes, callerID)
			if len(mutexes) == 0 {
				delete(d.volumeMountLocks, volumeName)
			}
			logrus.Debugf("unlocked volume %s for caller ID %s", volumeName, callerID)
		} else {
			return errors.New(fmt.Sprintf("cannot find locks for volume %s and caller ID %s", volumeName, callerID))
		}
	}
	return nil
}

func (d cephRBDVolumeDriver) mountLocksCount(pool, name string) int {
	if d.etcdLockSession != nil {
		volumeName := fmt.Sprintf("%s/%s", pool, name)
		if mutexes, found := d.volumeMountLocks[volumeName]; found {
			return len(mutexes)
		}
	}
	return 0
}

// Get the list of volumes registered with the plugin.
//
// POST /VolumeDriver.List
//
// Request:
//    {}
//    List the volumes mapped by this plugin.
//
// Response:
//    { "Volumes": [ { "Name": "volume_name", "Mountpoint": "/path/to/directory/on/host" } ], "Err": null }
//    Respond with an array containing pairs of known volume names and their
//    respective paths on the host filesystem (where the volumes have been
//    made available).
//
func (d cephRBDVolumeDriver) List() (*volume.ListResponse, error) {
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API LIST")
	return d.ListInternal()
}

func (d cephRBDVolumeDriver) ListInternal() (*volume.ListResponse, error) {
	logrus.Debugf("API ListInternal")

	logrus.Debugf("Retrieving all images from all RBD Pools")
	defaultImages, err := d.listImagesFromAllPools()
	if err != nil {
		logrus.Errorf("Error getting RBD images: %s", err)
		return nil, err
	}

	logrus.Debugf("Retrieving currently mounted volumes")
	volumes, err := d.currentVolumes()
	if err != nil {
		logrus.Errorf("Error retrieving currently mounted volumes: %s", err)
		return nil, err
	}

	var vols []*volume.Volume
	var vnames = make(map[string]int)

	for k, v := range volumes {
		var vname = fmt.Sprintf("%s/%s", v.Pool, v.Name)
		vnames[vname] = 1
		apiVol := &volume.Volume{Name: vname, Mountpoint: k}
		vols = append(vols, apiVol)
	}
	for _, v := range defaultImages {
		_, ok := vnames[v]
		if !ok {
			apiVol := &volume.Volume{Name: v}
			vols = append(vols, apiVol)
		}
	}

	logrus.Infof("Volumes found: %+v", vols)
	return &volume.ListResponse{Volumes: vols}, nil
}

// Get the volume info.
//
// POST /VolumeDriver.Get
//
// GetRequest:
//    { "Name": "volume_name" }
//    Docker needs reminding of the path to the volume on the host.
//
// GetResponse:
//    { "Volume": { "Name": "volume_name", "Mountpoint": "/path/to/directory/on/host" }}
//
func (d cephRBDVolumeDriver) Get(r *volume.GetRequest) (*volume.GetResponse, error) {
	d.m.Lock()
	defer d.m.Unlock()
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API GET(%s", r)
	return d.GetInternal(r)
}

func (d cephRBDVolumeDriver) GetInternal(r *volume.GetRequest) (*volume.GetResponse, error) {
	logrus.Debugf("API GetInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, opts, err := d.parseImagePoolName(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}

	var found *volume.Volume
	allVolumes, err1 := d.ListInternal()
	if err1 != nil {
		logrus.Errorf("error getting volume list: %s", err1)
		return nil, errors.New(fmt.Sprintf("Couldn't get volume info for %s/%s: %s", pool, name, err1))
	}
	for _, v := range allVolumes.Volumes {
		vpool, vname, _, errv := d.parseImagePoolName(v.Name)
		if errv != nil {
			logrus.Warnf("Error parsing image name %s. err=%s", v.Name, errv)
			continue
		}
		// var prname = fmt.Sprintf("%s/%s", pool, rname)
		// logrus.Debugf(">>>>> %s == %s %s ?", v.Name, r.Name, (v.Name == r.Name))
		// if v.Name == prname {
		// if v.Name == r.Name {
		// logrus.Debugf(">>>>> %s %s == %s %s ?", vpool, vname, pool, name)
		if vpool == pool && vname == name {
			found = v
			break
		}
	}

	if found != nil {
		logrus.Infof("Volume found for image %s/%s: %s", pool, name, found)
		volumeName := found.Name
		if opts != "" {
			volumeName = found.Name + "#" + opts
		}
		return &volume.GetResponse{Volume: &volume.Volume{Name: volumeName, Mountpoint: found.Mountpoint, CreatedAt: "2018-01-01T00:00:00-00:00"}}, nil
	} else {
		err := fmt.Sprintf("Volume not found for %s", r.Name)
		logrus.Infof("%s", err)
		return nil, errors.New(err)
	}
}

// Path returns the path to host directory mountpoint for volume.
//
// POST /VolumeDriver.Path
//
// Request:
//    { "Name": "volume_name" }
//    Docker needs reminding of the path to the volume on the host.
//
// Response:
//    { "Mountpoint": "/path/to/directory/on/host", "Err": null }
//    Respond with the path on the host filesystem where the volume has been
//    made available, and/or a string error if an error occurred.
//
// NOTE: this method does not require the Ceph connection
// FIXME: does volume API require error if Volume requested does not exist/is not mounted? Similar to List/Get leaving mountpoint empty?
//
func (d cephRBDVolumeDriver) Path(r *volume.PathRequest) (*volume.PathResponse, error) {
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API PATH(%s", r)
	return d.PathInternal(r)
}

func (d cephRBDVolumeDriver) PathInternal(r *volume.PathRequest) (*volume.PathResponse, error) {
	logrus.Debugf("API PathInternal(%s)", r)
	// parse full image name for optional/default pieces
	pool, name, opts, err := d.parseImagePoolName(r.Name)
	readonly := (opts == "ro")
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}

	mountpath := d.mountpoint(pool, name, readonly)

	if volumes, err := d.currentVolumes(); err != nil {
		logrus.Errorf("Error retrieving currently mounted volumes: %s", err)
		return nil, err
	} else {
		if _, ok := volumes[mountpath]; ok {
			logrus.Infof("Mountpath for volume %s/%s is %s", pool, name, mountpath)
			return &volume.PathResponse{Mountpoint: mountpath}, nil
		} else {
			err := fmt.Sprintf("Volume %s/%s not mounted at %s", pool, name, mountpath)
			logrus.Errorf("%s", err)
			return nil, errors.New(err)
		}
	}
}

// POST /VolumeDriver.Unmount
//
// - assuming writes are finished and no other containers using same disk on this host?

// Request:
//    { "Name": "volume_name", ID: "client-id" }
//    Indication that Docker no longer is using the named volume. This is
//    called once per container stop. Plugin may deduce that it is safe to
//    deprovision it at this point.
//
// Response:
//    Respond with error or nil
//
func (d cephRBDVolumeDriver) Unmount(r *volume.UnmountRequest) error {
	d.m.Lock()
	defer d.m.Unlock()
	logrus.Infof("")
	logrus.Infof(">>> DOCKER API UNMOUNT(%q)", r)
	return d.UnmountInternal(r)
}

func (d cephRBDVolumeDriver) UnmountInternal(r *volume.UnmountRequest) error {
	logrus.Debugf("API UnmountInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, opts, err := d.parseImagePoolName(r.Name)
	readonly := opts == "ro"
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	// release lock
	if err := d.unlockMountVolume(pool, name, r.ID); err != nil {
		return err
	}
	// continue to unmount only when there are no other locks for this mount
	if locksCount := d.mountLocksCount(pool, name); locksCount != 0 {
		logrus.Infof("skipping unmount... there are still %d locks for this mount", locksCount)
		return nil
	}

	mountpath := d.mountpoint(pool, name, readonly)
	logrus.Debugf("-------> MOUNT PATH: %s", mountpath)

	volumes, err := d.currentVolumes()
	if err != nil {
		err := fmt.Sprintf("Error retrieving currently mounted volumes: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	vol, found := volumes[mountpath]
	if !found {
		err := fmt.Sprintf("Volume %s/%s mount not found at %s", pool, name, mountpath)
		logrus.Errorf("%s", err)
		return errors.New(err)
	} else {
		logrus.Debugf("Volume %s/%s mount found at %s device %s. ", pool, name, mountpath, vol.Device)
	}

	// unmount
	// NOTE: this might succeed even if device is still in use inside container. device will disappear from host side but still be usable inside container :(
	logrus.Debugf("dismounting %s from device %s", mountpath, vol.Device)
	err = d.unmountPath(mountpath)
	if err != nil {
		err := fmt.Sprintf("Error dismounting device %s: %s", vol.Device, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
		// failsafe: will still attempt to unmap and unlock
		// logrus.Debugf("will try to unmap even with the failure of unmount")
	} else {
		logrus.Debugf("Volume %s/%s unmounted from %s device %s successfully. ", pool, name, mountpath, vol.Device)
	}

	// unmap
	logrus.Infof("Unmapping device %s from kernel for RBD Image %s/%s", vol.Device, pool, name)
	if err := d.unmapImageDevice(vol.Device); err != nil {
		logrus.Errorf("error unmapping image device %s: %s", vol.Device, err)
		// NOTE: rbd unmap exits 16 if device is still being used - unlike unmount.  try to recover differently in that case
		if rbdUnmapBusyRegexp.MatchString(err.Error()) {
			// can't always re-mount and not sure if we should here ... will be cleaned up once original container goes away
			err := fmt.Sprintf("unmap of device %s has failed due to 'busy device'", vol.Device)
			logrus.Errorf("%s", err)
			return errors.New(err)
		} else {
			err := fmt.Sprintf("unmap of device %s has failed: %s", vol.Device, err)
			logrus.Errorf("%s", err)
			return errors.New(err)
		}
		// other error, failsafe: proceed to attempt to unlock
		// return fmt.Sprintf("Error unmapping kernel device %s. err=%s", vol.Device, err)
	} else {
		logrus.Debugf("Volume %s/%s unmapped from kernel device %s successfully. ", pool, name, vol.Device)
	}

	//TODO CHECK LATER
	// // unlock
	// err = d.unlockImage(vol.Pool, vol.Name, vol.Locker)
	// if err != nil {
	// 	logrus.Errorf("unlocking RBD image(%s): %s", vol.Name, err)
	// 	err_msgs = append(err_msgs, "Error unlocking image")
	// }

	// logrus.Debugf("removing mount info from instance map")
	// delete(d.volumes, mountpath)
	return nil
}

// AUTHORIZATION DOCKER API

// AuthZReq is called when the Docker daemon receives an API request.
// All requests are allowed.
// func (d *cephRBDVolumeDriver) AuthZReq(r authorization.Request) authorization.Response {
// 	return authorization.Response{Allow: true}
// }

// AuthZRes is called before the Docker daemon returns an API response.
// All responses are allowed.
// func (d *cephRBDVolumeDriver) AuthZRes(r authorization.Request) authorization.Response {
// 	return authorization.Response{Allow: true}
// }

//
// END Docker VolumeDriver Plugin API methods
//
// ***************************************************************************
// ***************************************************************************
//

// rbdDefaultPoolImageList performs an `rbd ls` on the default pool
func (d cephRBDVolumeDriver) rbdDefaultPoolImageList() ([]string, error) {
	return d.rbdPoolImageList(d.defaultCephPool)
}

// rbdPoolImageList performs an `rbd ls` on the pool
func (d cephRBDVolumeDriver) rbdPoolImageList(pool string) ([]string, error) {
	result, err := d.rbdsh(pool, "ls")
	if err != nil {
		return nil, err
	}
	if result == "" {
		return nil, nil
	}
	// split into lines - should be one rbd image name per line
	return strings.Split(result, "\n"), nil
}

// listImagesFromAllPools list pools and its images
// returns array with 'poolName/imageName' items
func (d cephRBDVolumeDriver) listImagesFromAllPools() ([]string, error) {
	poolList, err := poolList()
	if err != nil {
		return nil, err
	}
	var allImages []string
	for _, pool := range poolList {
		images, err := d.rbdPoolImageList(pool)
		if err != nil {
			return nil, err
		}
		for _, image := range images {
			allImages = append(allImages, fmt.Sprintf("%s/%s", pool, image))
		}
	}
	return allImages, nil
}

// create ceph osd pool
// initialize created pool
func (d cephRBDVolumeDriver) createPool(pool string) error {
	if !d.canCreatePools {
		err := fmt.Sprintf("the pool '%s' does not exists and the cepher is not allowed to auto create it", pool)
		logrus.Error(err)
		return errors.New(err)
	}
	logrus.Infof("creating pool '%s'", pool)
	_, err := shWithDefaultTimeout("ceph", "osd", "pool", "create", pool, d.defaultPoolPgNum)
	if err != nil {
		err := fmt.Sprintf("error while creating pool '%s': %s", pool, err)
		logrus.Error(err)
		return errors.New(err)
	}
	logrus.Infof("initializing pool '%s'", pool)
	_, err = d.rbdsh(pool, "pool", "init", pool)
	if err != nil {
		err := fmt.Sprintf("error while initializing pool '%s': %s", pool, err)
		logrus.Error(err)
		return errors.New(err)
	}
	logrus.Infof("pool '%s' created successfully", pool)
	return nil
}

// poolList performs an `rbd ls` on the pool
func poolList() ([]string, error) {
	result, err := shWithDefaultTimeout("ceph", "osd", "pool", "ls")
	if err != nil {
		return nil, err
	}
	// split into lines - should be one pool name per line
	return strings.Split(result, "\n"), nil
}

func poolExists(pool string) (bool, error) {
	_, err := shWithDefaultTimeout("ceph", "osd", "pool", "get", pool, "size")
	if err != nil {
		// ENOENT = Error NO ENTry/ENTity
		if strings.Contains(err.Error(), "ENOENT") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// mountpoint returns the expected path on host
func (d cephRBDVolumeDriver) mountpoint(pool, name string, readonly bool) string {
	//TODO add random hash to avoid mountpoint reusage (so that locks will be performed per instance bind)?
	mp := filepath.Join(d.rootMountDir, pool, name)
	if readonly {
		mp = mp + ":ro"
	} else {
		mp = mp + ":rw"
	}
	return mp
}

// func (d *cephRBDVolumeDriver) parseImagePoolNameForCeph(fullname string) (pool string, imagename string, err error) {
// 	return d.parseImagePoolName(fullname, true)
// }

// func (d *cephRBDVolumeDriver) parseImagePoolNameForDocker(fullname string) (pool string, imagename string, err error) {
// 	return d.parseImagePoolName(fullname, false)
// }

func (d cephRBDVolumeDriver) parseImagePoolName(fullname string) (pool string, imagename string, opts string, err error) {
	//example matches:
	// Full match	0-17	`pool1/myimage1#ro`
	// Group 1.	0-6	`pool1/`
	// Group 2.	0-5	`pool1`
	// Group 3.	6-14	`myimage1`
	// Group 4.	14-17	`#ro`
	// Group 5.	15-17	`ro`

	// Full match	0-11	`myimage1#ro`
	// Group 3.	0-8	`myimage1`
	// Group 4.	8-11	`#ro`
	// Group 5.	9-11	`ro`

	// Full match	0-14	`pool1/myimage1`
	// Group 1.	0-6	`pool1/`
	// Group 2.	0-5	`pool1`
	// Group 3.	6-14	`myimage1`

	// Full match	0-8	`myimage1`
	// Group 3.	0-8	`myimage1`

	pool = d.defaultCephPool // defaul pool for plugin
	opts = ""

	imageNameRegexp := regexp.MustCompile(`^(([-_.[:alnum:]]+)/)?([-_.[:alnum:]]+)(#(ro))?$`)

	matches := imageNameRegexp.FindStringSubmatch(fullname)
	nmatches := len(matches)
	if nmatches < 2 {
		return "", "", "", errors.New("Unable to parse image name: " + fullname)
	} else if nmatches == 2 {
		imagename = matches[1]
	} else if nmatches == 4 {
		if strings.HasPrefix(matches[2], "#") {
			imagename = matches[1]
			opts = matches[3]
		} else {
			pool = matches[2]
			imagename = matches[3]
		}
	} else if nmatches == 6 {
		pool = matches[2]
		imagename = matches[3]
		opts = matches[5]
	}

	return pool, imagename, opts, nil
}

// rbdImageExists will check for an existing RBD Image
func (d cephRBDVolumeDriver) rbdImageExists(pool, findName string) (bool, error) {
	_, err := d.rbdsh(pool, "info", findName)
	if err != nil {
		// NOTE: even though method signature returns err - we take the error
		// in this instance as the indication that the image does not exist
		// TODO: can we double check exit value for exit status 2 ?
		logrus.Debugf("RBD image info returned an error: %s", err)
		return false, nil
	}
	return true, nil
}

// createRBDImage will create a new Ceph block device and make a filesystem on it
func (d cephRBDVolumeDriver) createRBDImage(pool string, name string, size int, fstype string, features string) error {
	logrus.Infof("Creating new RBD Image pool=%v; name=%v; size=%v; fs=%v; features=%v)", pool, name, size, fstype, features)

	// check that fs is valid type (needs mkfs.fstype in PATH)
	mkfs, err := exec.LookPath("mkfs." + fstype)
	if err != nil {
		msg := fmt.Sprintf("Unable to find mkfs for %s in PATH: %s", fstype, err)
		return errors.New(msg)
	}

	//prepare call
	ics := strings.Split(features, ",")
	cargs := make([]string, 0)
	cargs = append(cargs, name)
	cargs = append(cargs, []string{"--image-format", strconv.Itoa(2), "--size", strconv.Itoa(size)}...)
	for _, v := range ics {
		cargs = append(cargs, []string{"--image-feature", v}...)
	}

	// _, err = shWithDefaultTimeout("rbd", cargs...)

	//perform call
	_, err = d.rbdsh(pool, "create", cargs...)
	// "--image-format", strconv.Itoa(2),
	// "--size", strconv.Itoa(size),
	// "--image-feature", "layering",
	// "--image-feature", "striping",
	// "--image-feature", "exclusive-lock",
	// name)
	if err != nil {
		err := fmt.Sprintf("error creating RBD Image %s/%s: %s", pool, name, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	//TODO REVIEW LATER
	// // lock it temporarily for fs creation
	// lockname, err := d.lockImage(pool, name)
	// if err != nil {
	// 	return err
	// }

	logrus.Debugf("Mapping newly created image %s/%s to kernel device", pool, name)
	device, err := d.mapImageToDevice(pool, name, false)
	if err != nil {
		// defer d.unlockImage(pool, name, lockname)
		err := fmt.Sprintf("error mapping kernel device: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	} else {
		logrus.Debugf("Done")
	}

	logrus.Debugf("Formatting filesystem %s on device %s", fstype, device)
	// _, err = (5*time.Minute, mkfs, device)
	_, err = ExecShellTimeout(5*time.Minute, mkfs, device)
	if err != nil {
		defer d.unmapImageDevice(device)
		err := fmt.Sprintf("error formatting filesystem %s on device %s: %s", fstype, device, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	} else {
		logrus.Debugf("Done")
	}

	// TODO: should we chown/chmod the directory? e.g. non-root container users
	// won't be able to write. where to get the preferred user id?

	// unmap
	logrus.Debugf("Unmap device %s", device)
	err = d.unmapImageDevice(device)
	if err != nil {
		// ? if we cant unmap -- are we screwed? should we unlock?
		err := fmt.Sprintf("error unmapping device %s: %s", device, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	} else {
		logrus.Debugf("Done")
	}

	//TODO REVIEW LATER
	// // unlock
	// err = d.unlockImage(pool, name, lockname)
	// if err != nil {
	// 	return err
	// }
	logrus.Infof("RBD Image creation completed and filesystem prepared")

	return nil
}

// rbdImageIsLocked returns true if named image is already locked
// func (d *cephRBDVolumeDriver) rbdImageIsLocked(pool, name string) (bool, error) {
// 	// check the output for a lock -- if blank or error, assume not locked (?)
// 	out, err := d.rbdsh(pool, "lock", "ls", name)
// 	if err != nil || out != "" {
// 		return false, err
// 	}
// 	// otherwise - no error and output is not blank - assume a lock exists ...
// 	return true, nil
// }

// lockImage locks image and returns locker cookie name
// func (d *cephRBDVolumeDriver) lockImage(pool, imagename string) (string, error) {
// 	cookie := d.localLockerCookie()
// 	// _, err := d.rbdsh(pool, "lock", "add", imagename, cookie)
// 	// if err != nil {
// 	// 	return "", err
// 	// }
// 	log.Printf("SKIPPING LOCKIMAGE %s %s", pool, imagename)
// 	return cookie, nil
// }

// localLockerCookie returns the Hostname
// func (d *cephRBDVolumeDriver) localLockerCookie() string {
// 	host, err := os.Hostname()
// 	if err != nil {
// 		logrus.Warnf("HOST_UNKNOWN: unable to get hostname: %s", err)
// 		host = "HOST_UNKNOWN"
// 	}
// 	return host
// }

// unlockImage releases the exclusive lock on an image
// func (d *cephRBDVolumeDriver) unlockImage(pool, imagename, locker string) error {
// 	log.Printf("SKIPPING UNLOCKIMAGE %s %s %s", pool, imagename, locker)

// if locker == "" {
// 	logrus.Warnf("Attempting to unlock image(%s/%s) for empty locker using default hostname", pool, imagename)
// 	// try to unlock using the local hostname
// 	locker = d.localLockerCookie()
// }
// logrus.Infof("unlockImage(%s/%s, %s)", pool, imagename, locker)

// // first - we need to discover the client id of the locker -- so we have to
// // `rbd lock list` and grep out fields
// out, err := d.rbdsh(pool, "lock", "list", imagename)
// if err != nil || out == "" {
// 	logrus.Errorf("image not locked or ceph rbd error: %s", err)
// 	return err
// }

// // parse out client id -- assume we looking for a line with the locker cookie on it --
// var clientid string
// lines := grepLines(out, locker)
// if isDebugEnabled() {
// 	logrus.Debugf("found lines matching %s:\n%s\n", locker, lines)
// }
// if len(lines) == 1 {
// 	// grab first word of first line as the client.id ?
// 	tokens := strings.SplitN(lines[0], " ", 2)
// 	if tokens[0] != "" {
// 		clientid = tokens[0]
// 	}
// }

// if clientid == "" {
// 	return errors.New("sh_unlockImage: Unable to determine client.id")
// }

// _, err = d.rbdsh(pool, "lock", "rm", imagename, locker, clientid)
// if err != nil {
// 	return err
// }
// 	return nil
// }

// removeRBDImage will remove a RBD Image - no undo available
func (d cephRBDVolumeDriver) removeRBDImage(pool, name string) error {
	logrus.Infof("Deleting RBD Image %s/%s on Ceph Cluster", pool, name)

	// remove the block device image
	_, err := d.rbdsh(pool, "rm", name)

	if err != nil {
		err := fmt.Sprintf("error deleting RBD Image %s/%s: %s", pool, name, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}
	return nil
}

// renameRBDImage will move a RBD Image to new name
func (d cephRBDVolumeDriver) renameRBDImage(pool, name, newname string) error {
	logrus.Debugf("Rename RBD Image %s/%s to %s/%s", pool, name, pool, newname)

	_, err := d.rbdsh(pool, "rename", name, newname)
	if err != nil {
		err := fmt.Sprintf("error renaming RBD Image %s/%s to %s/%s: %s", pool, name, pool, newname, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}
	return nil
}

func (d cephRBDVolumeDriver) mapImageToDevice(pool string, imagename string, readonly bool) (string, error) {
	//map image to kernel device
	if d.useRBDKernelModule {
		logrus.Debugf("Mapping RBD image %s/%s using RBD Kernel module", pool, imagename)
		return d.rbdsh(pool, "map", imagename)
	} else {
		logrus.Debugf("Mapping RBD image %s/%s using nbd-rbd client. readonly=%v", pool, imagename, readonly)
		if !readonly {
			//during tests, rbd --exclusive guarantees only one mapping with --exclusive will take place for an image.
			//if the host is rebooted, the lock is released too. Right after unmap, the image is available for lock by another host immediatelly.
			//works very well for --exclusive x --exclusive competitions
			// return shWithDefaultTimeout("rbd-nbd", "--exclusive", "--timeout", "60", "map", pool+"/"+imagename)
			return shWithDefaultTimeout("rbd-nbd", "--exclusive", "map", pool+"/"+imagename)
		} else {
			//during tests, simultaneous mapping with --read-only is permitted, but
			//it allows --read-only to be placed while there is another --exclusive mapping, which is bad.
			//--exclusive while --read-only is in place works too (shouldn't!)
			if d.etcdLockSession != nil {
				// return shWithDefaultTimeout("rbd-nbd", "--read-only", "--timeout", "60", "map", pool+"/"+imagename)
				return shWithDefaultTimeout("rbd-nbd", "--read-only", "map", pool+"/"+imagename)
			} else {
				return "", errors.New("Only exclusive write access (single mapping of a volume) is supported at a time. For shared locks, specify a ETCD server for distributed RW Lock management (--lock-etcd)")
			}
		}
	}
}

// unmapImageDevice will release the mapped kernel device
func (d cephRBDVolumeDriver) unmapImageDevice(device string) error {
	//unmap device from kernel
	if d.useRBDKernelModule {
		logrus.Debugf("Unmapping device %s using RBD Kernel module", device)
		_, err := d.rbdsh("", "unmap", device)
		return err
	} else {
		logrus.Debugf("Unmapping device %s using rbd-rbd client", device)
		_, err := shWithDefaultTimeout("rbd-nbd", "--timeout", "60", "unmap", device)
		// _, err := shWithDefaultTimeout("rbd-nbd", "unmap", device)
		return err
	}
}

// list mapped kernel devices
func (d cephRBDVolumeDriver) listMappedDevices() ([]*Volume, error) {
	var devices string = ""
	if d.useRBDKernelModule {
		logrus.Debug("Listing mapped devices using RBD Kernel module")
		result, err := d.rbdsh("", "device", "list")
		if err != nil {
			return nil, err
		}
		devices = result
	} else {
		logrus.Debug("Listing mapped devices using rbd-nbd client")
		result, err := shWithDefaultTimeout("rbd-nbd", "list-mapped")
		if err != nil {
			logrus.Debugf("Error listing mapped devices. Maybe no devices found. Ignoring: %s", err)
		} else {
			devices = result
		}

		//doing something to list mapped devices inspired on https://github.com/ceph/ceph/blob/master/src/tools/rbd_nbd/rbd-nbd.cc
		// result1, err := shWithDefaultTimeout("find", "/sys/block/nbd*/pid")
		// if err != nil {
		// 	logrus.Debugf("Error listing mapped devices: %s", err)
		// }
		// re := regexp.MustCompile(`.*\/(nbd[0-9]+)\/pid`)
		// devices = re.ReplaceAllString(result1, "/dev/$1")
		// logrus.Debugf("result1: %s", result1)
		// var result3 string = ""
		// devices = result1
		// var lines = strings.Split(result1, "\n")
		// for _,v := range lines {
		// 	if(v == "") {
		// 		continue
		// 	}
		// 	result2, err := shWithDefaultTimeout("cat", v)
		// 	logrus.Debugf("result2: %s", result2)
		// 	if err != nil {
		// 		logrus.Debugf("Error listing mapped devices: %s", err)
		// 	}
		// 	if result2 != "0" {
		// 		result3 += v + "\n"
		// 	}
		// 	logrus.Debugf("result3: %s", result3)
		// }
	}

	logrus.Debugf("Mapped devices found: %s", devices)

	var mappings []*Volume

	var lines = strings.Split(devices, "\n")
	var header bool = false
	for _, v := range lines {
		if !header {
			header = true
		} else {
			var fields = spaceDelimitedFieldsRegexp.FindAllStringSubmatch(v, -1)
			logrus.Debugf("%s", fields)
			if fields != nil && len(fields) == 5 {
				var vol = &Volume{
					Pool:   fields[1][0],
					Name:   fields[2][0],
					Device: fields[4][0],
				}
				mappings = append(mappings, vol)
			} else {
				return nil, errors.New(fmt.Sprintf("Cannot get mapped devices from line %s", v))
			}
		}
	}

	return mappings, nil
}

// list mapped kernel devices
func (d cephRBDVolumeDriver) listMounts() ([]*Volume, error) {
	// NOTE: this does not even require a user nor a pool, just device name
	result, err := shWithDefaultTimeout("mount")
	if err != nil {
		return nil, err
	}

	var mounts []*Volume

	var lines = strings.Split(result, "\n")
	for _, v := range lines {
		var fields = spaceDelimitedFieldsRegexp.FindAllStringSubmatch(v, -1)
		if fields != nil && len(fields) >= 3 {
			var m = &Volume{
				Device:    fields[0][0],
				Mountpath: fields[2][0],
			}
			mounts = append(mounts, m)
		} else {
			return nil, errors.New(fmt.Sprintf("Cannot get mount fields from line %s", v))
		}
	}
	return mounts, nil
}

// Callouts to other unix shell commands: blkid, mount, umount

// deviceType identifies Image FS Type - requires RBD image to be mapped to kernel device
func (d cephRBDVolumeDriver) deviceType(device string) (string, error) {
	// blkid Output:
	//	xfs
	blkid, err := shWithDefaultTimeout("blkid", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		return "", err
	} else if blkid != "" {
		return blkid, nil
	} else {
		return "", errors.New("Unable to determine device fs type from blkid")
	}
}

// verifyDeviceFilesystem will attempt to check XFS filesystems for errors
func (d cephRBDVolumeDriver) checkDeviceFilesystem(device string, mountpath string, fstype string, readonly bool) error {
	logrus.Debugf("Checking filesystem %s on device %s", fstype, device)
	// for now we only handle XFS
	// TODO: use fsck for ext4?

	if fstype == "xfs" {
		// check XFS volume
		err := d.xfsRepairDryRun(device)
		if err != nil {
			switch err.(type) {
			case ShTimeoutError:
				// propagate timeout errors - can't recover? system error? don't try to mount at that point
				logrus.Debugf("Timeout checking filesystem")
				return err
			default:
				if !readonly {
					// assume any other error is xfs error and attempt limited repair
					return d.attemptLimitedXFSRepair(fstype, device, mountpath)
				} else {
					logrus.Warnf("Filesystem %s at %s seem to have errors but cannot be fixed because it is readonly", fstype, mountpath)
					return err
				}
			}
		}
	}

	return nil
}

func (d cephRBDVolumeDriver) xfsRepairDryRun(device string) error {
	// "xfs_repair  -n  (no  modify node) will return a status of 1 if filesystem
	// corruption was detected and 0 if no filesystem corruption was detected." xfs_repair(8)
	// TODO: can we check cmd output and ensure the mount/unmount is suggested by stale disk log?
	_, err := shWithDefaultTimeout("xfs_repair", "-n", device)
	return err
}

// attemptLimitedXFSRepair will try mount/unmount and return result of another xfs-repair-n
func (d cephRBDVolumeDriver) attemptLimitedXFSRepair(fstype, device, mountpath string) (err error) {
	logrus.Warnf("attempting limited XFS repair (mount/unmount) of %s %s", device, mountpath)

	// mount
	err = d.mountDeviceToPath(fstype, device, mountpath, false)
	if err != nil {
		return err
	}

	// unmount
	err = d.unmountPath(mountpath)
	if err != nil {
		return err
	}

	// try a dry-run again and return result
	return d.xfsRepairDryRun(device)
}

// mountDevice will call mount on kernel device with a docker volume subdirectory
func (d cephRBDVolumeDriver) mountDeviceToPath(fstype string, device string, path string, readonly bool) error {
	// if readonly {
	// 	// logrus.Infof("Path %s was mounted to %s in readonly mode. Make sure the mount options in Docker volume is :ro because the mount driver can't ensure the container won't write on a 'ro' mount (unfortunatelly!)", device, path)
	// 	path1 := path + ":rw"
	// 	err := os.MkdirAll(path1, os.ModeDir|os.FileMode(int(0775)))
	// 	if err != nil {
	// 		return err
	// 	}
	// 	//mount as rw for a workaround (mount with -o ro doesn't take effect)
	// 	_, err = shWithDefaultTimeout("mount", "-t", fstype, device, path1)
	// 	if err != nil {
	// 		return err
	// 	} else {
	// 		//now bind mount with readonly flag (ro option directly on the first mount doesn't work!)
	// 		_, err = shWithDefaultTimeout("mount", path1, path, "-o", "bind,ro")

	// 		return err
	// 	}
	// } else {
	_, err := shWithDefaultTimeout("mount", "-t", fstype, device, path)
	return err
	// }
}

// unmountDevice will call umount on kernel device to unmount from host's docker subdirectory
func (d cephRBDVolumeDriver) unmountPath(path string) error {
	_, err := shWithDefaultTimeout("umount", path)
	if err != nil {
		return err
	}
	return err
}

// rbdsh will call rbd with the given command arguments, also adding config, user and pool flags
func (d cephRBDVolumeDriver) rbdsh(pool, command string, args ...string) (string, error) {
	args = append([]string{"--conf", d.cephConfigFile, "--id", d.cephUser, command}, args...)
	if pool != "" {
		args = append([]string{"--pool", pool}, args...)
	}
	return shWithDefaultTimeout("rbd", args...)
}

func (d cephRBDVolumeDriver) isVolumeReadonly(volumeName string) (isRO bool, err error) {
	return regexp.MatchString("#ro", volumeName)
}

func (d cephRBDVolumeDriver) currentVolumes() (map[string]*Volume, error) {
	mapped, err := d.listMappedDevices()
	if err != nil {
		err := fmt.Sprintf("error getting mapped devices: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}
	logrus.Debugf("system mapped rbd kernel devices: %v", mapped)

	mounts, err := d.listMounts()
	if err != nil {
		err := fmt.Sprintf("error getting current mounts: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}
	logrus.Debugf("system mounts: %v", mounts)

	//transform array to map
	deviceToMountPathMap := make(map[string]string)
	volumes := make(map[string]*Volume)

	for _, m := range mounts {
		deviceToMountPathMap[m.Device] = m.Mountpath
	}

	for _, v := range mapped {
		mountpath, found := deviceToMountPathMap[v.Device]
		if found {
			//add detected mount point as initial mount state
			logrus.Debugf("RBD Image %s/%s found mounted at %s with device %s", v.Pool, v.Name, mountpath, v.Device)
			volumes[mountpath] = &Volume{
				Pool:      v.Pool,
				Name:      v.Name,
				Device:    v.Device,
				Mountpath: mountpath,
			}
		} else {
			logrus.Debugf("RBD Image %s/%s found mapped to device %s, but it is not mounted yet.", v.Pool, v.Name, v.Device)
			logrus.Debugf("unmapping device")
			err = d.unmapImageDevice(v.Device)
			if err != nil {
				logrus.Errorf("error on unmap of %s: %s", v.Device, err)
				return nil, errors.New(fmt.Sprintf("error on unmapping of unmounted kernel rbd device %s. RBD Image %s/%s: %s", v.Device, v.Pool, v.Name, err))
			} else {
				logrus.Debugf("unmap successful")
			}
		}
	}

	return volumes, nil
}
