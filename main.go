package main

import (
	_ "errors"
	_ "flag"
	_ "fmt"
	_ "log"
	_ "os"
	_ "os/signal"
	_ "path/filepath"
	_ "syscall"

	_ "github.com/sirupsen/logrus"
	_ "github.com/docker/go-plugins-helpers/authorization"
	_ "github.com/docker/go-plugins-helpers/sdk"
	_ "github.com/docker/go-plugins-helpers/volume"
	_ "github.com/etcd-io/etcd/contrib/recipes"
	_ "github.com/flaviostutz/etcd-lock/etcdlock"
	_ "go.etcd.io/etcd/clientv3"
	_ "go.etcd.io/etcd/clientv3/concurrency"
	_ "go.etcd.io/etcd/mvcc/mvccpb"
	// _ "go-plugins-helpers/volume"
)

func main() {
	// fmt.Println("This is used for build caching purposes. Should be replaced.")
}
