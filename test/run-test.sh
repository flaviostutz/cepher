#!/bin/bash

echo "Cleaning old files of previous build"
rm ./image-generate/go.*
rm ./image-generate/ceph.conf.template
rm -rf ./image-generate/cepher

echo "Coping new files to generate a new Build"
cp ../go.* ./image-generate
cp ../ceph.conf.template ./image-generate
cp -r ../cepher ./image-generate

echo "Build the images.."
docker-compose build

echo "Running test.."
HOST_IP=172.20.10.10 docker-compose up