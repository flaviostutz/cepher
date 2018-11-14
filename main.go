package main

import (
	_ "errors"
	_ "flag"
	"fmt"
	_ "log"
	_ "os"
	_ "os/signal"
	_ "path/filepath"
	_ "syscall"

	_ "github.com/Sirupsen/logrus"
	_ "github.com/docker/go-plugins-helpers/volume"
	// _ "go-plugins-helpers/volume"
)

func main() {
	fmt.Println("This is used for build caching purposes. Should be replaced.")
}
