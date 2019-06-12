# TESTING CEPHER PLUGIN

Testing the cepher plugin will make the CRUD (Create/Mount/Umount/Remove) operations. This is necessary at every update of your Ceph cluster, because there are some functions that can stop working or deprecate, or at development time.

## Latest deployment configuration
 
 - Ubuntu 18.04 with kernel 4.15.0-50 (default)
 - Docker 18.09.6
 - docker-compose 1.23.2
 - Ceph 13.2.5


## How to

** MAC Users: to test this, is necessary one VM with Ubuntu with remote access to execute the host mode of docker **

1. Discover the IP Address of the machine where you docker** is running.

2. Run the docker compose file *docker-compose.ceph.yml* with the command above, changing the *XXX.XXX.XXX.XXX* for the discovered IP Address

```
HOST_IP=XXX.XXX.XXX.XXX docker-compose -f docker-compose.ceph.yml up -d
```

3. Run the shell script *run-test.sh* with the command:

```
bash run-test.sh
```

This shell script will create a container with the an ceph-client and will execute:

* List of all Ceph images created at the pool volumes
* 6 concurrent operations of CRUD 

