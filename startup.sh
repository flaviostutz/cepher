#!/bin/bash
set -e
set -x

if [ "$CEPH_MONITOR_HOST" == "" ]; then
    echo "CEPH_MONITOR_HOST is mandatory"
    exit 1
fi

resolveKeyring() {
    if [ -f /etc/ceph/keyring ]; then
        echo "Monitor key already known"
        return 0
    elif [ "$ETCD_URL" != "" ]; then 
        echo "Retrieving monitor key from ETCD /$CEPH_CLUSTER_NAME/keyring..."
        KEYRING=$(etcdctl --endpoints $ETCD_URL get "/$CEPH_CLUSTER_NAME/keyring")
        if [ $? -eq 0 ]; then
            echo $KEYRING > /tmp/base64keyring
            base64 -d -i /tmp/base64keyring > /etc/ceph/keyring
            return 0
        else
            return 2
        fi
    else
        echo "Monitor key doesn't exist and ETCD was not defined. Cannot retrieve keys."
        return 1
    fi
}

if [ "$CEPH_AUTH" == "cephx" ]; then
    if [ "$ETCD_URL" != "" ]; then
        while true; do
            resolveKeyring && break
            echo "Retrying in 1s..."
            sleep 1
        done
        if [ "$?" != 0 ]; then
            echo "Couldn't get a keyring from ETCD. Aborting."
            exit 2
        fi
    elif [ "$CEPH_KEYRING_BASE64" != "" ]; then
        echo $CEPH_KEYRING_BASE64 > /tmp/base64keyring
        base64 -d -i /tmp/base64keyring > /etc/ceph/keyring
        if [ "$?" != 0 ]; then
            echo "Couldn't parse the keyring from CEPH_KEYRING_BASE64. Aborting."
            exit 2
        fi
    else
        echo "You have to set ETCD_HOST or pass the contents of your keyring file encoded in base64 in CEPH_KEYRING_BASE64 so we can get a keyring and connect to Ceph cluster"
        exit 1
    fi
fi 

export MOUNT_PATH="/mnt/rbd-driver/$PLUGIN_NAME"
export RBD_DOCKER_PLUGIN_DEBUG=$LOG_DEBUG

mkdir -p $MOUNT_PATH

echo "Creating ceph.conf..."
cat /ceph.conf.template | envsubst > /etc/ceph/ceph.conf
cat /etc/ceph/ceph.conf

echo "Preparing default Ceph pool $DEFAULT_POOL_NAME..."
set +e
R=$(ceph osd pool ls | grep ${DEFAULT_POOL_NAME} -x)
set -e
if [ "$R" != "" ]; then
    echo "Pool was found in Ceph cluster"
else
    echo "Pool was not found in Ceph cluster"
    if [ "$DEFAULT_POOL_CREATE" == "true" ]; then
        echo "Creating pool ${DEFAULT_POOL_NAME}..."
        osd pool default pg num = ${DEFAULT_POOL_PG_NUM}
        if [ "$DEFAULT_POOL_QUOTA_MAX_BYTES" != "" ]; then
            echo "Setting quota max bytes to ${DEFAULT_POOL_QUOTA_MAX_BYTES}..."
            ceph osd pool set-quota data max_bytes ${DEFAULT_POOL_QUOTA_MAX_BYTES}
        fi
    fi
fi
rbd pool init ${DEFAULT_POOL_NAME}

echo "Starting rbd-docker-plugin..."
rbd-docker-plugin \
    --name $PLUGIN_NAME \
    --user $CEPH_USER \
    --cluster $CEPH_CLUSTER_NAME \
    --pool $DEFAULT_POOL_NAME \
    --mount $MOUNT_PATH \
    --create $CAN_AUTO_CREATE_VOLUMES \
    --fs $DEFAULT_IMAGE_FS \
    --size $DEFAULT_IMAGE_SIZE \
    --config /etc/ceph/ceph.conf

