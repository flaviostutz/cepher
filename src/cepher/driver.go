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

	"github.com/docker/go-plugins-helpers/volume"
	"github.com/Sirupsen/logrus"
    // "go-plugins-helpers/volume"
)

var (
	imageNameRegexp    = regexp.MustCompile(`^(([-_.[:alnum:]]+)/)?([-_.[:alnum:]]+)(@([0-9]+))?$`) // optional pool or size in image name
	rbdUnmapBusyRegexp = regexp.MustCompile(`^exit status 16$`)
	spaceDelimitedFieldsRegexp = regexp.MustCompile(`([^\s]+)`)
)

// Volume is our local struct to store info about RBD Image
type Volume struct {
	Pool   string
	Name   string // RBD Image name
	Device string // local host kernel device (e.g. /dev/rbd1)
	Mountpath string
}

// our driver type for impl func
type cephRBDVolumeDriver struct {
	// - using default ceph cluster name ("ceph")
	// - using default ceph config (/etc/ceph/<cluster>.conf)

	// name    string             // unique name for plugin
	cluster string             // ceph cluster to use (default: ceph)
	user    string             // ceph user to use (default: admin)
	pool    string             // ceph pool to use (default: rbd)
	root    string             // scratch dir for mounts for this plugin
	config  string             // ceph config file to read
	m       *sync.Mutex        // mutex to guard operations that change volume maps or use conn
}

// newCephRBDVolumeDriver builds the driver struct, reads config file and connects to cluster
func newCephRBDVolumeDriver(cluster, userName, defaultPoolName, mountDir, config string) cephRBDVolumeDriver {
	// the root mount dir will be based on docker default root and plugin name - pool added later per volume
	logrus.Debugf("setting base mount dir to %s", mountDir)

	// fill everything except the connection and context
	driver := cephRBDVolumeDriver{
		cluster: cluster,
		user:    userName,
		pool:    defaultPoolName,
		root:    mountDir,
		config:  config,
		m:       &sync.Mutex{},
	}

	return driver
}

func (d cephRBDVolumeDriver) init() error {
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
	logrus.Infof(">>>>>>>> DOCKER API CREATE(%q)", r)
	d.m.Lock()
	defer d.m.Unlock()
	return d.CreateInternal(r)
}

func (d cephRBDVolumeDriver) CreateInternal(r *volume.CreateRequest) error {
	logrus.Debugf("CreateInternal(%q)", r)

	fstype := *defaultImageFSType

	// parse image name optional/default pieces
	pool, name, size, err := d.parseImagePoolNameSize(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	// Options to override from `docker volume create -o OPT=VAL ...`
	if r.Options["pool"] != "" {
		pool = r.Options["pool"]
	}
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

	logrus.Debugf("verify if image already exists on RBD cluster")
	exists, err := d.rbdImageExists(pool, name)
	if err != nil {
		err := fmt.Sprintf("error while checking RBD Image %d/%s: %s", pool, name, err)
		logrus.Errorf("%s", err)
		return errors.New(err)

	} else if !exists {
		logrus.Debugf("Ceph Image doesn't exist yet")
		if *canCreateVolumes {
			logrus.Debugf("create image on RBD Cluster")
			err = d.createRBDImage(pool, name, size, fstype)
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
	logrus.Infof(">>>>>>>> DOCKER API REMOVE(%q)", r)
	d.m.Lock()
	defer d.m.Unlock()
	return d.RemoveInternal(r)
}

func (d cephRBDVolumeDriver) RemoveInternal(r *volume.RemoveRequest) error {
	logrus.Debugf("API RemoveInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, _, err := d.parseImagePoolNameSize(r.Name)
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
		logrus.Debugf("RBD Image %s/%s exists. Proceeding to removal using action '%s'", pool, name, removeActionFlag)
	}

	// // attempt to gain lock before remove - lock seems to disappear after rm (but not after rename)
	// locker, err := d.lockImage(pool, name)
	// if err != nil {
	// 	errString := fmt.Sprintf("Unable to lock image for remove: %s", name)
	// 	logrus.Errorf(errString)
	// 	return errors.New(errString)
	// }

	// remove action can be: ignore, delete or rename
	if removeActionFlag == "delete" {
		logrus.Debugf("Deleting RBD Image %s/%s from Ceph Cluster", pool, name)
		err = d.removeRBDImage(pool, name)
		if err != nil {
			errString := fmt.Sprintf("Unable to remove RBD Image %s/%s: %s", pool, name, err)
			logrus.Errorf(errString)
			// defer d.unlockImage(pool, name, locker)
			return errors.New(errString)
		}

		// defer d.unlockImage(pool, name, locker)
	} else if removeActionFlag == "rename" {
		logrus.Debugf("Renaming RBD Image %s/%s in Ceph Cluster to zz_ prefix", pool, name)
		// TODO: maybe add a timestamp?
		err = d.renameRBDImage(pool, name, "zz_"+name)
		if err != nil {
			errString := fmt.Sprintf("Unable to rename RBD Image %s/%s with zz_ prefix: %s", pool, name, err)
			logrus.Errorf(errString)
			// unlock by old name
			// defer d.unlockImage(pool, name, locker)
			return errors.New(errString)
		} else {
			logrus.Infof("RBD Image %s/%s renamed successfully to %s/zz_%s", pool, name, pool, name)
		}
		// unlock by new name
		// defer d.unlockImage(pool, "zz_"+name, locker)
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
	logrus.Infof(">>>>>>>> DOCKER API MOUNT(%q)", r)
	d.m.Lock()
	defer d.m.Unlock()
	return d.MountInternal(r)
}

func (d cephRBDVolumeDriver) MountInternal(r *volume.MountRequest) (*volume.MountResponse, error) {
	logrus.Debugf("API MountInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, _, err := d.parseImagePoolNameSize(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}

	mountpath := d.mountpoint(pool, name)

	volumes, err := d.currentVolumes()
	if err!=nil {
		logrus.Errorf("Error retrieving currently mounted volumes: %s", err)
		return nil, err

	} else {
		_, found := volumes[mountpath]
		//volume already mounted
		if found {
			// err := fmt.Sprintf("")
			// logrus.Errorf("%s", err)
			// return nil, errors.New(err)
			logrus.Infof("Mountpoint %s already exists. Reusing it. pool=%s image=%s", mountpath, pool, name)

		//volume not mounted yet. mount!
		} else {
			logrus.Infof("Mountpoint %s doesn't exist yet. Creating it. pool=%s image=%s", mountpath, pool, name)

			// map
			logrus.Debugf("mapping kernel device to RBD Image")
			device, err := d.mapImage(pool, name)
			if err != nil {
				logrus.Errorf("error mapping RBD Image %s/%s to kernel device: %s", pool, name, err)
				// failsafe: need to release lock
				// defer d.unlockImage(pool, name, locker)
				return nil, errors.New(fmt.Sprintf("Unable to map kernel device. err=%s", err))
			}

			// determine device FS type
			fstype, err := d.deviceType(device)
			if err != nil {
				logrus.Warnf("unable to detect RBD Image %s/%s fstype: %s", name, err)
				// NOTE: don't fail - FOR NOW we will assume default plugin fstype
				fstype = *defaultImageFSType
			}

			// double check image filesystem if possible
			err = d.verifyDeviceFilesystem(device, mountpath, fstype)
			if err != nil {
				logrus.Errorf("filesystem at RBD Image %s/%s may need repairs: %s", pool, name, err)
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
			err = d.mountDevice(fstype, device, mountpath)
			if err != nil {
				logrus.Errorf("error mounting device %s to directory %s: %s", device, mountpath, err)
				logrus.Debugf("unmapping device")
				defer d.unmapImageDevice(device)
				// defer d.unlockImage(pool, name, locker)
				return nil, errors.New(fmt.Sprintf("Unable to mount device", err))
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
	logrus.Infof(">>>>>>>> DOCKER API LIST")
	return d.ListInternal();
}

func (d cephRBDVolumeDriver) ListInternal() (*volume.ListResponse, error) {
	logrus.Debugf("API ListInternal")

	logrus.Debugf("Retrieving all images from default RBD Pool %s", d.pool)
	defaultImages, err := d.rbdList()
	if err != nil {
		logrus.Errorf("Error getting images from RBD Pool %s: %s", d.pool, err)
		return nil, err
	}

	var vols[]*volume.Volume

	var vnames map[string]int
	vnames = make(map[string]int)

	logrus.Debugf("Retrieving currently mounted volumes")
	volumes, err := d.currentVolumes()
	if err!=nil {
		logrus.Errorf("Error retrieving currently mounted volumes: %s", err)
		return nil, err

	} else {
		for k,v := range volumes {
			var vname = fmt.Sprintf("%s/%s", v.Pool, v.Name)
			vnames[vname] = 1
			apiVol := &volume.Volume{Name: vname, Mountpoint: k}
			vols = append(vols, apiVol)
		}
	}

	for _,v := range defaultImages {
		var vname = fmt.Sprintf("%s/%s", d.pool, v)
		_, ok := vnames[vname]
		if(!ok) {
			apiVol := &volume.Volume{Name: vname}
			vols = append(vols, apiVol)
		}
	}

	logrus.Infof("Volumes found: %s", vols)
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
	logrus.Infof(">>>>>>>> DOCKER API GET(%s", r)
	return d.GetInternal(r)
}

func (d cephRBDVolumeDriver) GetInternal(r *volume.GetRequest) (*volume.GetResponse, error) {
	logrus.Debugf("API GetInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, _, err := d.parseImagePoolNameSize(r.Name)
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
	for _,v := range allVolumes.Volumes {
		_, rname, _, _ := d.parseImagePoolNameSize(r.Name)
		var prname = fmt.Sprintf("%s/%s", pool, rname)
		if(v.Name == prname) {
			found = v
			break
		}
	}

	if found != nil {
		logrus.Infof("Volume found for image %s/%s: %s", pool, name, found)
		return &volume.GetResponse{Volume: &volume.Volume{Name: found.Name, Mountpoint: found.Mountpoint, CreatedAt: "2018-01-01T00:00:00-00:00"}}, nil

	} else {
		err := fmt.Sprintf("Volume not found for %s/%s", pool, name)
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
	logrus.Infof(">>>>>>>> DOCKER API PATH(%s", r)
	return d.PathInternal(r)
}

func (d cephRBDVolumeDriver) PathInternal(r *volume.PathRequest) (*volume.PathResponse, error) {
	logrus.Debugf("API PathInternal(%s)", r)
	// parse full image name for optional/default pieces
	pool, name, _, err := d.parseImagePoolNameSize(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}

	mountpath := d.mountpoint(pool, name)

	volumes, err := d.currentVolumes()
	if err!=nil {
		logrus.Errorf("Error retrieving currently mounted volumes: %s", err)
		return nil, err

	} else {
		_, ok := volumes[mountpath]
		if ok {
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
	logrus.Infof(">>>>>>>> DOCKER API UNMOUNT(%s", r)
	d.m.Lock()
	defer d.m.Unlock()
	return d.UnmountInternal(r)
}

func (d cephRBDVolumeDriver) UnmountInternal(r *volume.UnmountRequest) error {
	logrus.Debugf("API UnmountInternal(%s)", r)

	// parse full image name for optional/default pieces
	pool, name, _, err := d.parseImagePoolNameSize(r.Name)
	if err != nil {
		err := fmt.Sprintf("error parsing volume name: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}

	mountpath := d.mountpoint(pool, name)

	volumes, err := d.currentVolumes()
	if err!=nil {
		err := fmt.Sprintf("Error retrieving currently mounted volumes: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)

	} else {
		vol, found := volumes[mountpath]
		if !found {
			err := fmt.Sprintf("Volume %s/%s mount not found at %s", pool, name, mountpath)
			logrus.Errorf("%s", err)
			return errors.New(err)

		} else {
			logrus.Debugf("Volume %s/%s mount found at %s device %s. ", pool, name, mountpath, vol.Device)
		}

		// unmount
		// NOTE: this might succeed even if device is still in use inside container. device will dissappear from host side but still be usable inside container :(
		logrus.Debugf("Unmounting %s from device %s", mountpath, vol.Device)
		err = d.unmountDevice(vol.Device)
		if err != nil {
			err := fmt.Sprintf("Error unmounting device %s: %s", vol.Device, err)
			logrus.Errorf("%s", err)
			return errors.New(err)
			// failsafe: will still attempt to unmap and unlock
			// logrus.Debugf("will try to unmap even with the failure of unmount")
		} else {
			logrus.Debugf("Volume %s/%s unmounted from %s device %s successfully. ", pool, name, mountpath, vol.Device)
		}

		// unmap
		logrus.Infof("Unmapping device %s from kernel for RBD Image %s/%s", vol.Device, pool, name)
		err = d.unmapImageDevice(vol.Device)
		if err != nil {
			logrus.Errorf("error unmapping image device %s: %s", vol.Device, err)
			// NOTE: rbd unmap exits 16 if device is still being used - unlike umount.  try to recover differently in that case
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
}

//
// END Docker VolumeDriver Plugin API methods
//
// ***************************************************************************
// ***************************************************************************
//



// rbdList performs an `rbd ls` on the default pool
func (d *cephRBDVolumeDriver) rbdList() ([]string, error) {
	result, err := d.rbdsh(d.pool, "ls")
	if err != nil {
		return nil, err
	}
	// split into lines - should be one rbd image name per line
	return strings.Split(result, "\n"), nil
}

// mountpoint returns the expected path on host
func (d *cephRBDVolumeDriver) mountpoint(pool, name string) string {
	return filepath.Join(d.root, pool, name)
}

// parseImagePoolNameSize parses out any optional parameters from Image Name
// passed from docker run. Fills in unspecified options with default pool or
// size.
//
// Returns: pool, image-name, size, error
//
func (d *cephRBDVolumeDriver) parseImagePoolNameSize(fullname string) (pool string, imagename string, size int, err error) {
	// Examples of regexp matches:
	//   foo: ["foo" "" "" "foo" "" ""]
	//   foo@1024: ["foo@1024" "" "" "foo" "@1024" "1024"]
	//   pool/foo: ["pool/foo" "pool/" "pool" "foo" "" ""]
	//   pool/foo@1024: ["pool/foo@1024" "pool/" "pool" "foo" "@1024" "1024"]
	//
	// Match indices:
	//   0: matched string
	//   1: pool with slash
	//   2: pool no slash
	//   3: image name
	//   4: size with @
	//   5: size only
	//
	matches := imageNameRegexp.FindStringSubmatch(fullname)
	// if isDebugEnabled() {
	// 	logrus.Debugf("parseImagePoolNameSize: \"%s\": %q", fullname, matches)
	// }
	if len(matches) != 6 {
		return "", "", 0, errors.New("Unable to parse image name: " + fullname)
	}

	// 2: pool
	pool = d.pool // defaul pool for plugin
	if matches[2] != "" {
		pool = matches[2]
	}

	// 3: image
	imagename = matches[3]

	// 5: size
	size = *defaultImageSizeMB
	if matches[5] != "" {
		var err error
		size, err = strconv.Atoi(matches[5])
		if err != nil {
			logrus.Warnf("using default. unable to parse int from %s: %s", matches[5], err)
			size = *defaultImageSizeMB
		}
	}

	return pool, imagename, size, nil
}

// rbdImageExists will check for an existing RBD Image
func (d *cephRBDVolumeDriver) rbdImageExists(pool, findName string) (bool, error) {
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
func (d *cephRBDVolumeDriver) createRBDImage(pool string, name string, size int, fstype string) error {
	logrus.Infof("Creating new RBD Image pool=%s; name=%s; size=%s; fs=%s)", pool, name, size, fstype)

	// check that fs is valid type (needs mkfs.fstype in PATH)
	mkfs, err := exec.LookPath("mkfs." + fstype)
	if err != nil {
		msg := fmt.Sprintf("Unable to find mkfs for %s in PATH: %s", fstype, err)
		return errors.New(msg)
	}

	logrus.Debugf("Creating RBD Image %s/%s in Ceph cluster with features layering, striping and exclusive-lock", pool, name)
	_, err = d.rbdsh(
		pool, "create",
		"--image-format", strconv.Itoa(2),
		"--size", strconv.Itoa(size),
		"--image-feature", "layering", 
		"--image-feature", "striping", 
		"--image-feature", "exclusive-lock",
		name)
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
	device, err := d.mapImage(pool, name)
	if err != nil {
		// defer d.unlockImage(pool, name, lockname)
		err := fmt.Sprintf("error mapping kernel device: %s", err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	} else {
		logrus.Debugf("Done")
	}

	logrus.Debugf("Formatting filesystem %s on device %s", fstype, device)
	_, err = shWithTimeout(5*time.Minute, mkfs, device)
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
func (d *cephRBDVolumeDriver) removeRBDImage(pool, name string) error {
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
func (d *cephRBDVolumeDriver) renameRBDImage(pool, name, newname string) error {
	logrus.Debugf("Rename RBD Image %s/%s to %s/%s", pool, name, pool, newname)

	_, err := d.rbdsh(pool, "rename", name, newname)
	if err != nil {
		err := fmt.Sprintf("error renaming RBD Image %s/%s to %s/%s: %s", pool, name, pool, newname, err)
		logrus.Errorf("%s", err)
		return errors.New(err)
	}
	return nil
}

// mapImage will map the RBD Image to a kernel device
func (d *cephRBDVolumeDriver) mapImage(pool, imagename string) (string, error) {
	logrus.Debugf("Mapping RBD image %s/%s to kernel device", pool, imagename)
	return d.rbdsh(pool, "map", imagename)
}

// unmapImageDevice will release the mapped kernel device
func (d *cephRBDVolumeDriver) unmapImageDevice(device string) error {
	// NOTE: this does not even require a user nor a pool, just device name
	_, err := d.rbdsh("", "unmap", device)
	return err
}

// list mapped kernel devices
func (d *cephRBDVolumeDriver) listMappedDevices() ([]*Volume, error) {
	// NOTE: this does not even require a user nor a pool, just device name
	result, err := d.rbdsh("", "device", "list")
	if err != nil {
		return nil, err
	}

	var mappings[]*Volume

	var lines = strings.Split(result, "\n")
	var header bool = false
	for _,v := range lines {
		if !header {
			header = true
		} else {
			var fields = spaceDelimitedFieldsRegexp.FindAllStringSubmatch(v, -1)
			logrus.Debugf("%s", fields)
			if fields != nil && len(fields)==5 {
				var vol = &Volume {
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
func (d *cephRBDVolumeDriver) listMounts() ([]*Volume, error) {
	// NOTE: this does not even require a user nor a pool, just device name
	result, err := shWithDefaultTimeout("mount")
	if err != nil {
		return nil, err
	}

	var mounts[]*Volume

	var lines = strings.Split(result, "\n")
	for _,v := range lines {
		var fields = spaceDelimitedFieldsRegexp.FindAllStringSubmatch(v, -1)
		if fields != nil && len(fields)>=3 {
			var m = &Volume {
								Device:     fields[0][0],
								Mountpath:  fields[2][0],
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
func (d *cephRBDVolumeDriver) deviceType(device string) (string, error) {
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
func (d *cephRBDVolumeDriver) verifyDeviceFilesystem(device, mount, fstype string) error {
	logrus.Debugf("Checking filesystem %s on device %s", fstype, device)
	// for now we only handle XFS
	// TODO: use fsck for ext4?
	if fstype != "xfs" {
		return nil
	}

	// check XFS volume
	err := d.xfsRepairDryRun(device)
	if err != nil {
		switch err.(type) {
		case ShTimeoutError:
			// propagate timeout errors - can't recover? system error? don't try to mount at that point
			logrus.Debugf("Timeout checking filesystem")
			return err
		default:
			// assume any other error is xfs error and attempt limited repair
			return d.attemptLimitedXFSRepair(fstype, device, mount)
		}
	}

	return nil
}

func (d *cephRBDVolumeDriver) xfsRepairDryRun(device string) error {
	// "xfs_repair  -n  (no  modify node) will return a status of 1 if filesystem
	// corruption was detected and 0 if no filesystem corruption was detected." xfs_repair(8)
	// TODO: can we check cmd output and ensure the mount/unmount is suggested by stale disk log?
	_, err := shWithDefaultTimeout("xfs_repair", "-n", device)
	return err
}

// attemptLimitedXFSRepair will try mount/unmount and return result of another xfs-repair-n
func (d *cephRBDVolumeDriver) attemptLimitedXFSRepair(fstype, device, mount string) (err error) {
	logrus.Warnf("attempting limited XFS repair (mount/unmount) of %s %s", device, mount)

	// mount
	err = d.mountDevice(fstype, device, mount)
	if err != nil {
		return err
	}

	// unmount
	err = d.unmountDevice(device)
	if err != nil {
		return err
	}

	// try a dry-run again and return result
	return d.xfsRepairDryRun(device)
}

// mountDevice will call mount on kernel device with a docker volume subdirectory
func (d *cephRBDVolumeDriver) mountDevice(fstype, device, mountdir string) error {
	_, err := shWithDefaultTimeout("mount", "-t", fstype, device, mountdir)
	return err
}

// unmountDevice will call umount on kernel device to unmount from host's docker subdirectory
func (d *cephRBDVolumeDriver) unmountDevice(device string) error {
	_, err := shWithDefaultTimeout("umount", device)
	return err
}

// rbdsh will call rbd with the given command arguments, also adding config, user and pool flags
func (d *cephRBDVolumeDriver) rbdsh(pool, command string, args ...string) (string, error) {
	args = append([]string{"--conf", d.config, "--id", d.user, command}, args...)
	if pool != "" {
		args = append([]string{"--pool", pool}, args...)
	}
	return shWithDefaultTimeout("rbd", args...)
}

func (d *cephRBDVolumeDriver) currentVolumes() (map[string]*Volume, error) {
	mapped, err := d.listMappedDevices()
	if err != nil {
		err := fmt.Sprintf("error getting mapped devices: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}
	logrus.Debugf("system mapped rbd kernel devices: %s", mapped)

	mounts, err := d.listMounts()
	if err != nil {
		err := fmt.Sprintf("error getting current mounts: %s", err)
		logrus.Errorf("%s", err)
		return nil, errors.New(err)
	}
	logrus.Debugf("system mounts: %s", mounts)

	//transform array to map
	var deviceToMountPathMap map[string]string
	deviceToMountPathMap = make(map[string]string)

	volumes := make(map[string]*Volume)

	for _,m := range mounts {
		deviceToMountPathMap[m.Device] = m.Mountpath
	}

	for _,v := range mapped {
		mountpath, found := deviceToMountPathMap[v.Device]
		if found {
			//add detected mount point as initial mount state
			logrus.Debugf("RBD Image %s/%s found mounted at %s with device %s", v.Pool, v.Name, mountpath, v.Device)
			volumes[mountpath] = &Volume{
				Pool:   v.Pool,
				Name:   v.Name,
				Device: v.Device,
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