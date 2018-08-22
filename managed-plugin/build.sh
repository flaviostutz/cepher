#!/bin/bash

set -e
set -x

docker rmi -f cepher-rootfsimage || true
docker build -t cepher-rootfsimage ../
id=$(docker create cepher-rootfsimage true)
mkdir -p rootfs
docker export "$id" | tar -x --exclude=dev/* -C rootfs
docker rm -vf "$id"
docker rmi -f cepher-rootfsimage

docker plugin rm -f flaviostutz/cepher || true
docker plugin create flaviostutz/cepher .

echo "For pushing this plugin to repo, 'docker plugin push flaviostutz/cepher'"
