#!/bin/bash

set -x
set -e

source ../.env

docker plugin install flaviostutz/cepher \
  --grant-all-permissions \
  --alias=cepher \
  MONITOR_HOSTS="${HOST_IP}:16789,${HOST_IP}:26789,${HOST_IP}:16789" \
  ETCD_URL="http://${HOST_IP}:12379" \
  LOG_LEVEL=debug \
  DEFAULT_IMAGE_SIZE=1 \
  ENABLE_AUTO_CREATE_VOLUMES=true
