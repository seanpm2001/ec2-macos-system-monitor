package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	ec2sm "github.com/aws/ec2-macos-system-monitor/lib/ec2macossystemmonitor"
)

// pollInterval is the duration in between gathering of CPU metrics
const pollInterval = 60 * time.Second

// DefaultSerialDevice is the default serial device attached to mac1.metal instances for communication
// This device is able to receive various payloads when encapsulated in json
const DefaultSerialDevice = "/dev/cu.pci-serial0"

func main() {
	disableSyslog := flag.Bool("disable-syslog", false, "Prevent log output to syslog")
	flag.Parse()

	logger, err := ec2sm.NewLogger("ec2monitoring-cpuutilization", !*disableSyslog, true)
	if err != nil {
		log.Fatalf("Failed to create logger: %s", err)
	}

	logger.Infof("Starting up relayd for monitoring\n")
	relay, err := ec2sm.NewRelay(DefaultSerialDevice)
	if err != nil {
		log.Fatalf("Failed to create relay: %s", err)
	}
	intervalString := strconv.Itoa(ec2sm.DefaultLogInterval)
	cpuStatus := ec2sm.StatusLogBuffer{Message: "Sent CPU Utilization (%d bytes) over " + intervalString + " minute(s)", Written: 0}
	relayStatus := ec2sm.StatusLogBuffer{Message: "[relayd] Received data and sent %d bytes to serial device over " + intervalString + " minutes", Written: 0}

	// Kick off Relay in a go routine
	go relay.StartRelay(logger, &relayStatus)

	// Setup signal handling into a channel, catch SIGINT and SIGTERM for now which should suffice for launchd
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	// Setup the polling channel for kicking off CPU metrics gathering
	pollingCh := time.Tick(pollInterval)

	// Setup logging interval timer for flushing logs
	LoggingTimer := time.Tick(ec2sm.DefaultLogInterval * time.Minute)

	// Check if the socket is there, if not, warn that this might fail
	if !ec2sm.CheckSocketExists() {
		logger.Fatal("Socket does not exist, relayd may not be running")
	}
	// Main for loop that polls for signals and CPU ticks
	for {
		select {
		case sig := <-signals:
			if cpuStatus.Written > 0 {
				logger.Infof(cpuStatus.Message, cpuStatus.Written)
			}
			if relayStatus.Written > 0 {
				logger.Infof(relayStatus.Message, relayStatus.Written)
			}
			log.Println("exiting due to signal:", sig)
			// Send signal to relay server through channel to shutdown
			relay.ReadyToClose <- true
			// Exit cleanly
			os.Exit(0)
		case <-pollingCh:
			// Fetch the current CPU Utilization
			cpuUtilization, err := ec2sm.RunningCpuUsage()
			if err != nil {
				logger.Fatalf("Unable to get CPU Utilization: %s\n", err)
			}

			// Send the data to the relay
			written, err := ec2sm.SendMessage("cpuutil", cpuUtilization, false)
			if err != nil {
				logger.Fatalf("Unable to write message to relay: %s", err)
			}
			// Add current written values to running total cpuStatus.Written
			cpuStatus.Written += int64(written)
		case <-LoggingTimer:
			// flush the logs since the timer fired. The cpuStatus info is local to this routine but relayStatus is not,
			// so use atomic for the non-local one to ensure its safe
			logger.Infof(cpuStatus.Message, cpuStatus.Written)
			// Since we logged the total, reset to zero for continued tracking
			cpuStatus.Written = 0
			logger.Infof(relayStatus.Message, relayStatus.Written)
			// Since we logged the total, reset to zero, do this via atomic since its modified in another goroutine
			atomic.StoreInt64(&relayStatus.Written, 0)
		}

	}
}
