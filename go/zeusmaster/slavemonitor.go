package zeusmaster

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	slog "github.com/burke/zeus/go/shinylog"
	"github.com/burke/zeus/go/unixsocket"
)

type SlaveMonitor struct {
	tree *ProcessTree
}

func StartSlaveMonitor(tree *ProcessTree, quit chan bool) {
	monitor := &SlaveMonitor{tree}

	localMasterFile, remoteMasterFile, err := unixsocket.Socketpair(syscall.SOCK_DGRAM)
	if err != nil {
		Error("Couldn't create socketpair")
	}

	localMasterSocket, err := unixsocket.NewUsockFromFile(localMasterFile)
	if err != nil {
		Error("Couldn't Open UNIXSocket")
	}

	// We just want this unix socket to be a channel so we can select on it...
	registeringFds := make(chan int, 3)
	go func() {
		for {
			fd, err := localMasterSocket.ReadFD()
			if err != nil {
				slog.Error(err)
			}
			registeringFds <- fd
		}
	}()

	for _, slave := range monitor.tree.SlavesByName {
		if slave.Parent == nil {
			go monitor.startInitialProcess(remoteMasterFile)
		} else {
			go monitor.bootSlave(slave)
		}
	}

	for {
		select {
		case <-quit:
			quit <- true
			monitor.cleanupChildren()
			return
		case fd := <-registeringFds:
			monitor.slaveDidBeginRegistration(fd)
		}
	}
}

func (mon *SlaveMonitor) cleanupChildren() {
	for _, slave := range mon.tree.SlavesByName {
		slave.Shutdown()
	}
}

func (mon *SlaveMonitor) bootSlave(slave *SlaveNode) {
	for {
		slave.Parent.WaitUntilBooted()

		msg := CreateSpawnSlaveMessage(slave.Name)
		unixsocket.NewUsock(slave.Parent.Socket).WriteMessage(msg)

		restartNow := make(chan bool)
		go func() {
			slave.Parent.WaitUntilUnbooted()
			restartNow <- true
		}()
		go func() {
			slave.WaitUntilRestartRequested()
			restartNow <- true
		}()

		<-restartNow
		slave.Kill()
	}
}

func (mon *SlaveMonitor) startInitialProcess(sock *os.File) {
	for {
		command := mon.tree.ExecCommand
		parts := strings.Split(command, " ")
		executable := parts[0]
		args := parts[1:]
		cmd := exec.Command(executable, args...)
		cmd.Env = append(os.Environ(), fmt.Sprintf("ZEUS_MASTER_FD=%d", sock.Fd()))
		cmd.ExtraFiles = []*os.File{sock}

		// We want to let this process run "forever", but it will eventually
		// die... either on program termination or when its dependencies change
		// and we kill it. when it's requested to restart, err is "signal 9",
		// and we do nothing.
		go func() {
			output, err := cmd.CombinedOutput()
			if err == nil {
				ErrorConfigCommandCrashed(string(output))
			}
			msg := err.Error()
			if len(msg) > 11 && err.Error()[:11] != "exit status" {
				ErrorConfigCommandCouldntStart(err.Error())
			}
		}()

		restartNow := make(chan bool)
		go func() {
			mon.tree.Root.WaitUntilRestartRequested()
			restartNow <- true
		}()

		<-restartNow
		mon.tree.Root.Kill()
	}
}

func (mon *SlaveMonitor) slaveDidBeginRegistration(fd int) {
	// Having just started the process, we expect an IO, which we convert to a UNIX domain socket
	fileName := strconv.Itoa(rand.Int())
	slaveFile := unixsocket.FdToFile(fd, fileName)
	slaveUsock, err := unixsocket.NewUsockFromFile(slaveFile)
	if err != nil {
		slog.Error(err)
	}
	if err = slaveUsock.Conn.SetReadBuffer(1024); err != nil {
		slog.Error(err)
	}
	if err = slaveUsock.Conn.SetWriteBuffer(1024); err != nil {
		slog.Error(err)
	}

	// We now expect the slave to use this fd they send us to send a Pid&Identifier Message
	msg, err := slaveUsock.ReadMessage()
	if err != nil {
		slog.Error(err)
	}
	pid, identifier, err := ParsePidMessage(msg)

	slaveNode := mon.tree.FindSlaveByName(identifier)
	if slaveNode == nil {
		Error("slavemonitor.go:slaveDidBeginRegistration:Unknown identifier:" + identifier)
	}

	go slaveNode.Run(identifier, pid, slaveUsock)
}
