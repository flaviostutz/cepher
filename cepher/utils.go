//This is a hard fork from the great job done by
//http://github.com/yp-engineering/rbd-docker-plugin
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-cmd/cmd"
	"github.com/sirupsen/logrus"
)

var (
	defaultShellTimeout = 2 * 60 * time.Second
)

// returns current user gid or 0
func currentGid() int {
	gid := 0
	current, err := user.Current()
	if err != nil {
		return 0
	}
	gid, err = strconv.Atoi(current.Gid)
	if err != nil {
		return 0
	}

	return gid
}

// sh is a simple os.exec Command tool, returns trimmed string output
func sh(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	logrus.Debugf("sh CMD: %v", cmd)
	// TODO: capture and output STDERR to logfile?
	out, err := cmd.Output()
	return strings.Trim(string(out), " \n"), err
}

// ShResult used for channel in timeout
type ShResult struct {
	Output string // STDOUT
	Err    error  // go error, not STDERR
}

//ShTimeoutError used for sh timeout
type ShTimeoutError struct {
	timeout time.Duration
}

func (e ShTimeoutError) Error() string {
	return fmt.Sprintf("Reached TIMEOUT on shell command")
}

// shWithDefaultTimeout will use the defaultShellTimeout so you dont have to pass one
func shWithDefaultTimeout(name string, args ...string) (string, error) {
	// return shWithTimeout(defaultShellTimeout, name, args...)
	return ExecShellTimeout(defaultShellTimeout, name, args...)
}

// shWithTimeout will run the Cmd and wait for the specified duration
func ShWithTimeout(howLong time.Duration, name string, args ...string) (string, error) {
	// duration can't be zero
	if howLong <= 0 {
		return "", fmt.Errorf("Timeout duration needs to be positive")
	}
	// set up the results channel
	resultsChan := make(chan ShResult, 1)
	logrus.Debugf(">>>>EXEC: %s, %v", name, args)

	// fire up the goroutine for the actual shell command
	go func() {
		out, err := sh(name, args...)
		resultsChan <- ShResult{Output: out, Err: err}
	}()

	select {
	case res := <-resultsChan:
		return res.Output, res.Err
	case <-time.After(howLong):
		return "", ShTimeoutError{timeout: howLong}
	}
}

// grepLines pulls out lines that match a string (no regex ... yet)
func grepLines(data string, like string) []string {
	var result = []string{}
	if like == "" {
		logrus.Errorf("unable to look for empty pattern")
		return result
	}
	likeBytes := []byte(like)

	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), likeBytes) {
			result = append(result, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		logrus.Warnf("error scanning string for %s: %s", like, err)
	}

	return result
}

//ExecShellTimeout execute shell command with timeout
func ExecShellTimeout(timeout time.Duration, command string, args ...string) (string, error) {
	logrus.Debugf("shell command: %s", command)
	if len(args) > 0 {
		command = command + " " + strings.Join(args, " ")
	}
	acmd := cmd.NewCmd("bash", "-c", command)
	statusChan := acmd.Start() // non-blocking
	running := true
	// if ctx != nil {
	// 	ctx.CmdRef = acmd
	// }

	//kill if taking too long
	if timeout > 0 {
		logrus.Debugf("Enforcing timeout %s", timeout)
		go func() {
			startTime := time.Now()
			for running {
				if time.Since(startTime) >= timeout {
					logrus.Warnf("Stopping command execution because it is taking too long (%d seconds)", time.Since(startTime))
					acmd.Stop()
				}
				time.Sleep(1 * time.Second)
			}
		}()
	}

	// logrus.Debugf("Waiting for command to finish...")
	<-statusChan
	// logrus.Debugf("Command finished")
	running = false

	out := GetCmdOutput(acmd)
	status := acmd.Status()
	logrus.Debugf("shell output (%d): %s", status.Exit, out)
	if status.Exit != 0 {
		return out, fmt.Errorf("Failed to run command: '%s'; exit=%d; out=%s", command, status.Exit, out)
	}
	return out, nil
}

//GetCmdOutput return content of executed command
func GetCmdOutput(cmd *cmd.Cmd) string {
	status := cmd.Status()
	out := strings.Join(status.Stdout, "\n")
	if len(status.Stderr) > 0 {
		if len(out) > 0 {
			out = out + "\n" + strings.Join(status.Stderr, "\n")
		} else {
			out = strings.Join(status.Stderr, "\n")
		}
	}
	return out
}

func GenerateImageBackupName(name string, nameList []string) (string, error) {
	backupPrefix := "trash"
	count := 0
	backupNamePattern, err := regexp.Compile(fmt.Sprintf("^%s_([0-9]{1,3})_%s$", backupPrefix, name))
	if err != nil {
		return "", err
	}
	for _, imageName := range nameList {
		submatch := backupNamePattern.FindStringSubmatch(imageName)
		//Full match and group 1 are returned if matched
		if len(submatch) == 2 {
			backupNumber, err := strconv.Atoi(submatch[1])
			if err != nil {
				return "", err
			}
			if backupNumber >= count {
				count = backupNumber + 1
			}
		}
	}
	return fmt.Sprintf("%s_%d_%s", backupPrefix, count, name), nil
}
