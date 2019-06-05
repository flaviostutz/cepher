package main

import (
	"fmt"
	"testing"
	"time"
)

func TestExecuteCommand(t *testing.T) {
	test, err := ExecShellTimeout(5*time.Second, "lsg")
	if err != nil {
		fmt.Println("Error --> ", err)
	} else {
		fmt.Println("Executed Command")
		fmt.Println(test)
	}
	test2, err2 := ShWithTimeout(5*time.Second, "lsg")
	if err2 != nil {
		fmt.Println("Error --> ", err2)
	} else {
		fmt.Println("Executed Command")
		fmt.Println(test2)
	}
}

func TestExecuteCommandDefaultTimeout(t *testing.T) {
	test, err := shWithDefaultTimeout("curl", "http://localhost:3001/teste", "-H", "'Authorization: Bearer token'")
	if err != nil {
		fmt.Println("Error --> ", err)
	} else {
		fmt.Println("Executed Command")
		fmt.Println(test)
	}
}
