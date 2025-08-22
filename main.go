package main

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Host struct {
	IP        string
	MountPath string
	Port      int
	RemoteDir string
	Username  string
}

type HostResult struct {
	Host          Host
	Reachable     bool
	PingTime      time.Duration
	CheckTime     time.Duration
	Mounted       bool
	MountTime     time.Duration
	ExecutedCmd   string
	Error         error
	RemoteInfo    RemoteInfo
}

type RemoteInfo struct {
	Hostname string
	Uptime   string
	MAC      string
}

const (
	HOSTS_FILE     = "./sshfs_hosts.txt"
	MOUNT_BASE     = "/root"
	MOUNT_OPTIONS  = "cache=no,attr_timeout=0,entry_timeout=0"
	TIMEOUT        = 3
	CHECK_INTERVAL = 30
	LOG_FILE       = "/var/log/sshfs-monitor.log"
	PID_FILE       = "/var/run/sshfs-monitor.pid"
)

var (
	daemonMode   = false
	logFile      *os.File
	colorReset   = "\033[0m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorBlue    = "\033[34m"
	colorMagenta = "\033[35m"
	colorCyan    = "\033[36m"
	colorWhite   = "\033[37m"
	colorBold    = "\033[1m"
	colorDim     = "\033[2m"
	bgRed        = "\033[41m"
	bgGreen      = "\033[42m"
	bgYellow     = "\033[43m"
	bgBlue       = "\033[44m"
	bgCyan       = "\033[46m"
)

func initLogging() error {
	var err error
	logFile, err = os.OpenFile(LOG_FILE, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	log.SetOutput(logFile)
	return nil
}

func logMessage(message string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logEntry := fmt.Sprintf("[%s] %s", timestamp, message)
	if daemonMode && logFile != nil {
		log.Println(message)
	}
	if !daemonMode {
		fmt.Println(logEntry)
	}
}

func loadHosts() ([]Host, error) {
	file, err := os.Open(HOSTS_FILE)
	if err != nil {
		return nil, fmt.Errorf("error opening hosts file %s: %v", HOSTS_FILE, err)
	}
	defer file.Close()

	var hosts []Host
	scanner := bufio.NewScanner(file)
	
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		// Extract username and host
		var username, hostIP string
		if strings.Contains(parts[0], "@") {
			splitHost := strings.SplitN(parts[0], "@", 2)
			username = splitHost[0]
			hostIP = splitHost[1]
		} else {
			username = "root"
			hostIP = parts[0]
		}

		host := Host{
			IP:        hostIP,
			MountPath: parts[1],
			Port:      22,
			RemoteDir: "/root",
			Username:  username,
		}

		// Handle mount path
		if !filepath.IsAbs(host.MountPath) {
			host.MountPath = filepath.Join(MOUNT_BASE, host.MountPath)
		}

		// Handle port
		if len(parts) > 2 {
			if port, err := strconv.Atoi(parts[2]); err == nil {
				host.Port = port
			}
		}

		// Handle remote directory
		if len(parts) > 3 {
			host.RemoteDir = parts[3]
		}

		hosts = append(hosts, host)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading hosts file: %v", err)
	}

	if len(hosts) == 0 {
		return nil, fmt.Errorf("no hosts found in %s", HOSTS_FILE)
	}

	return hosts, nil
}

func pingHost(host string, timeout int) (bool, time.Duration) {
	start := time.Now()
	cmd := exec.Command("ping", "-c", "1", "-W", strconv.Itoa(timeout), host)
	err := cmd.Run()
	duration := time.Since(start)
	return err == nil, duration
}

func getPingTime(host string) string {
	cmd := exec.Command("ping", "-c", "1", "-W", strconv.Itoa(TIMEOUT), host)
	output, err := cmd.Output()
	if err != nil {
		return "N/A"
	}
	
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "time=") {
			parts := strings.Split(line, "time=")
			if len(parts) > 1 {
				timeStr := strings.Fields(parts[1])[0]
				return timeStr
			}
		}
	}
	return "N/A"
}

func clearStaleEndpoint(mountPoint string) error {
	// Check if directory exists
	if _, err := os.Stat(mountPoint); os.IsNotExist(err) {
		return nil
	}

	// Try to access the directory
	cmd := exec.Command("ls", mountPoint)
	if err := cmd.Run(); err != nil {
		if !daemonMode {
			fmt.Printf("Detected stale SSHFS endpoint at %s, clearing...\n", mountPoint)
		} else {
			logMessage(fmt.Sprintf("Detected stale SSHFS endpoint at %s, clearing...", mountPoint))
		}
		
		// Try fusermount first
		cmd = exec.Command("fusermount", "-u", mountPoint)
		if err := cmd.Run(); err != nil {
			// Try umount
			cmd = exec.Command("umount", mountPoint)
			if err := cmd.Run(); err != nil {
				// Try lazy umount
				cmd = exec.Command("umount", "-l", mountPoint)
				cmd.Run()
			}
		}
		
		time.Sleep(time.Second)
		
		// Verify cleanup
		cmd = exec.Command("ls", mountPoint)
		if err := cmd.Run(); err == nil {
			if !daemonMode {
				fmt.Printf("Successfully cleared stale endpoint: %s\n", mountPoint)
			} else {
				logMessage(fmt.Sprintf("Successfully cleared stale endpoint: %s", mountPoint))
			}
		} else {
			if !daemonMode {
				fmt.Printf("Warning: Could not fully clear stale endpoint: %s\n", mountPoint)
			} else {
				logMessage(fmt.Sprintf("Warning: Could not fully clear stale endpoint: %s", mountPoint))
			}
		}
	}
	
	return nil
}

func getRemoteInfo(host Host, infoType string) string {
	// Check if mounted first
	cmd := exec.Command("mountpoint", "-q", host.MountPath)
	if err := cmd.Run(); err != nil {
		return "N/A"
	}

	var sshCmd string
	switch infoType {
	case "uptime":
		sshCmd = "uptime | sed 's/.*up \\([^,]*\\).*/\\1/' | xargs"
	case "hostname":
		sshCmd = "hostname"
	case "mac":
		sshCmd = "cat /sys/class/net/eth0/address 2>/dev/null || ip link show eth0 2>/dev/null | grep -o '[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]' | head -1"
	default:
		return "N/A"
	}

	cmd = exec.Command("ssh", "-p", strconv.Itoa(host.Port), "-o", "ConnectTimeout=2", 
		"-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", host.Username, host.IP), sshCmd)
	output, err := cmd.Output()
	if err != nil {
		return "N/A"
	}
	
	result := strings.TrimSpace(string(output))
	if result == "" {
		return "N/A"
	}
	return result
}

func getLocalInfo(infoType string) string {
	var cmd *exec.Cmd
	switch infoType {
	case "hostname":
		cmd = exec.Command("hostname")
	case "mac":
		cmd = exec.Command("sh", "-c", "cat /sys/class/net/eth0/address 2>/dev/null || ip link show eth0 2>/dev/null | grep -o '[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]' | head -1")
	case "uptime":
		cmd = exec.Command("sh", "-c", "uptime | sed 's/.*up \\([^,]*\\).*/\\1/' | xargs")
	default:
		return "N/A"
	}
	
	output, err := cmd.Output()
	if err != nil {
		return "N/A"
	}
	return strings.TrimSpace(string(output))
}

func mountHost(host Host) HostResult {
	start := time.Now()
	
	result := HostResult{
		Host: host,
	}

	// Check if host is reachable
	reachable, pingDuration := pingHost(host.IP, TIMEOUT)
	result.Reachable = reachable
	result.PingTime = pingDuration
	result.CheckTime = time.Since(start)

	if !reachable {
		if daemonMode {
			logMessage(fmt.Sprintf("Host %s not reachable", host.IP))
		}
		return result
	}

	pingTimeStr := getPingTime(host.IP)
	if daemonMode {
		logMessage(fmt.Sprintf("Host %s reachable (ping: %s)", host.IP, pingTimeStr))
	}

	// Clear stale endpoints
	clearStaleEndpoint(host.MountPath)

	// Create mount directory if it doesn't exist
	if err := os.MkdirAll(host.MountPath, 0755); err != nil {
		result.Error = fmt.Errorf("failed to create mount directory: %v", err)
		return result
	}

	// Check if already mounted
	cmd := exec.Command("mountpoint", "-q", host.MountPath)
	if err := cmd.Run(); err == nil {
		// Verify mount is accessible
		cmd = exec.Command("ls", host.MountPath)
		if err := cmd.Run(); err == nil {
			result.Mounted = true
			result.ExecutedCmd = "already_mounted"
			if daemonMode {
				logMessage(fmt.Sprintf("Mount verified: %s", host.MountPath))
			}
			// Get remote info
			result.RemoteInfo = RemoteInfo{
				Hostname: getRemoteInfo(host, "hostname"),
				Uptime:   getRemoteInfo(host, "uptime"),
				MAC:      getRemoteInfo(host, "mac"),
			}
			return result
		}
		// Stale mount, clean it
		clearStaleEndpoint(host.MountPath)
	}

	// Mount the filesystem
	mountStart := time.Now()
	sshfsCmd := fmt.Sprintf("sshfs %s@%s:%s/ %s -o %s,port=%d",
		host.Username, host.IP, host.RemoteDir, host.MountPath, MOUNT_OPTIONS, host.Port)
	
	result.ExecutedCmd = sshfsCmd
	
	cmd = exec.Command("sshfs", 
		fmt.Sprintf("%s@%s:%s/", host.Username, host.IP, host.RemoteDir),
		host.MountPath,
		"-o", fmt.Sprintf("%s,port=%d", MOUNT_OPTIONS, host.Port))
	
	err := cmd.Run()
	result.MountTime = time.Since(mountStart)
	
	if err != nil {
		result.Error = fmt.Errorf("failed to mount: %v", err)
		msg := fmt.Sprintf("Failed to mount: %s:%d (%.6fs)", host.IP, host.Port, result.MountTime.Seconds())
		if daemonMode {
			logMessage(msg)
		}
		return result
	}

	result.Mounted = true
	msg := fmt.Sprintf("Successfully mounted: %s:%d -> %s (%.6fs)", host.IP, host.Port, host.MountPath, result.MountTime.Seconds())
	if daemonMode {
		logMessage(msg)
	}

	// Get remote info after successful mount
	result.RemoteInfo = RemoteInfo{
		Hostname: getRemoteInfo(host, "hostname"),
		Uptime:   getRemoteInfo(host, "uptime"),
		MAC:      getRemoteInfo(host, "mac"),
	}

	return result
}

func processHostsParallel(hosts []Host) []HostResult {
	var wg sync.WaitGroup
	results := make([]HostResult, len(hosts))
	
	for i, host := range hosts {
		wg.Add(1)
		go func(index int, h Host) {
			defer wg.Done()
			results[index] = mountHost(h)
		}(i, host)
	}
	
	wg.Wait()
	return results
}

func getStatusBadge(result HostResult) string {
	if result.Reachable {
		if result.Mounted {
			// Check if mount is still accessible
			cmd := exec.Command("mountpoint", "-q", result.Host.MountPath)
			if err := cmd.Run(); err == nil {
				return fmt.Sprintf("%s%s%s ONLINE  %s", bgGreen, colorBlue, colorBold, colorReset)
			} else {
				return fmt.Sprintf("%s%s%s STALE   %s", bgYellow, colorBlue, colorBold, colorReset)
			}
		} else {
			return fmt.Sprintf("%s%s%s CONN-ERR%s", bgYellow, colorBlue, colorBold, colorReset)
		}
	} else {
		return fmt.Sprintf("%s%s%s OFFLINE %s", bgRed, colorWhite, colorBold, colorReset)
	}
}

func printBootstrapStatus(results []HostResult) {
	// Clear screen and move cursor to top
	fmt.Print("\033[?25l\033[H\033[2J")
	
	localHostname := getLocalInfo("hostname")
	localUptime := getLocalInfo("uptime")
	localMAC := getLocalInfo("mac")
	
	// Header
	fmt.Printf("%s%s╔══════════════════════════════════════════════════════════════╗%s\n", colorBold, colorCyan, colorReset)
	fmt.Printf("%s%s║                    SSHFS STATUS MONITOR                     ║%s\n", colorBold, colorCyan, colorReset)
	
	// Local info
	localInfo := fmt.Sprintf("  Local: %s | Uptime: %s", localHostname, localUptime)
	localPadding := 62 - len(localInfo)
	fmt.Printf("%s%s║%s%s%s║%s\n", colorBold, colorCyan, localInfo, strings.Repeat(" ", localPadding), colorBold, colorReset)
	
	macInfo := fmt.Sprintf("  MAC: %s", localMAC)
	macPadding := 62 - len(macInfo)
	fmt.Printf("%s%s║%s%s%s║%s\n", colorBold, colorCyan, macInfo, strings.Repeat(" ", macPadding), colorBold, colorReset)
	
	fmt.Printf("%s%s╚══════════════════════════════════════════════════════════════╝%s\n", colorBold, colorCyan, colorReset)
	fmt.Println()
	
	totalHosts := len(results)
	onlineHosts := 0
	
	for i, result := range results {
		if result.Reachable {
			onlineHosts++
		}
		
		badge := getStatusBadge(result)
		hostLabel := fmt.Sprintf("Host %d", i+1)
		
		var pingDisplay string
		if result.Reachable {
			pingDisplay = fmt.Sprintf("%.3fms", float64(result.PingTime.Nanoseconds())/1e6)
		} else {
			pingDisplay = "N/A"
		}
		
		if result.Reachable {
			if result.Mounted {
				// Get disk usage
				cmd := exec.Command("df", "-h", result.Host.MountPath)
				var usage string
				if output, err := cmd.Output(); err == nil {
					lines := strings.Split(string(output), "\n")
					if len(lines) > 1 {
						fields := strings.Fields(lines[1])
						if len(fields) >= 5 {
							usagePercent := strings.TrimSuffix(fields[4], "%")
							if percent, err := strconv.Atoi(usagePercent); err == nil {
								// Create usage bar
								barLength := percent / 10
								usageBar := ""
								for j := 0; j < 10; j++ {
									if j < barLength {
										usageBar += "█"
									} else {
										usageBar += "░"
									}
								}
								usage = fmt.Sprintf("[%s %s%%]", usageBar, usagePercent)
							}
						}
					}
				}
				if usage == "" {
					usage = "[N/A]"
				}
				
				fmt.Printf("  %s %s (%s@%s) | Host: %s | Ping: %s | Mount: %s %s | Up: %s\n", 
					badge, hostLabel, result.Host.Username, result.Host.IP, result.RemoteInfo.Hostname, pingDisplay, result.Host.MountPath, usage, result.RemoteInfo.Uptime)
				fmt.Printf("    %s└─ MAC: %s%s\n", colorDim, result.RemoteInfo.MAC, colorReset)
			} else {
				fmt.Printf("  %s %s (%s@%s) | Host: %s | Ping: %s | Mount: Failed to connect | Up: %s\n", 
					badge, hostLabel, result.Host.Username, result.Host.IP, result.RemoteInfo.Hostname, pingDisplay, result.RemoteInfo.Uptime)
				fmt.Printf("    %s└─ MAC: %s%s\n", colorDim, result.RemoteInfo.MAC, colorReset)
			}
		} else {
			fmt.Printf("  %s %s (%s@%s) | Host: N/A | Ping: N/A | Mount: Not available | Up: N/A\n", 
				badge, hostLabel, result.Host.Username, result.Host.IP)
			fmt.Printf("    %s└─ MAC: N/A%s\n", colorDim, colorReset)
		}
	}
	
	// Summary
	fmt.Printf("%s%s┌─ SUMMARY ─────────────────────────────────────────────────────┐%s\n", colorBold, colorBlue, colorReset)
	
	successRate := 0
	if totalHosts > 0 {
		successRate = (onlineHosts * 100) / totalHosts
	}
	summaryInfo := fmt.Sprintf(" Total: %d hosts │ Online: %d hosts │ Success: %d%%", totalHosts, onlineHosts, successRate)
	summaryPadding := 62 - len(summaryInfo)
	fmt.Printf("%s%s│%s%s%s│%s\n", colorBold, colorBlue, summaryInfo, strings.Repeat(" ", summaryPadding), colorBold, colorReset)
	
	fmt.Printf("%s%s└───────────────────────────────────────────────────────────────┘%s\n", colorBold, colorBlue, colorReset)
	fmt.Println()
	fmt.Printf("%sLast updated: %s%s\n", colorDim, time.Now().Format("2006-01-02 15:04:05"), colorReset)
	
	if daemonMode {
		fmt.Printf("%sNext check in: %ds | Press Ctrl+C to stop%s\n", colorDim, CHECK_INTERVAL, colorReset)
	}
	
	// Show cursor
	fmt.Print("\033[?25h")
}

func printStats(results []HostResult, totalTime time.Duration) {
	fmt.Println()
	fmt.Println("==================== SSHFS CONNECTION STATS ====================")
	fmt.Printf("%-18s %-12s %-12s %-15s %-15s\n", "HOST", "STATUS", "PING (ms)", "PING TIME", "MOUNT TIME")
	fmt.Println("------------------------------------------------------------------")
	
	totalHosts := len(results)
	reachableHosts := 0
	mountedHosts := 0
	
	for _, result := range results {
		status := "UNREACHABLE"
		pingTimeStr := "N/A"
		mountStatus := "N/A"
		
		if result.Reachable {
			status = "REACHABLE"
			reachableHosts++
			pingTimeStr = fmt.Sprintf("%.3f", float64(result.PingTime.Nanoseconds())/1e6)
			
			if result.Mounted {
				if result.ExecutedCmd == "already_mounted" {
					mountStatus = "ALREADY MOUNTED"
				} else {
					mountStatus = fmt.Sprintf("SUCCESS (%.6fs)", result.MountTime.Seconds())
				}
				mountedHosts++
			} else if result.Error != nil {
				mountStatus = fmt.Sprintf("FAILED (%.6fs)", result.MountTime.Seconds())
			}
		}
		
		fmt.Printf("%-18s %-12s %-12s %-15s %-15s\n",
			fmt.Sprintf("%s@%s", result.Host.Username, result.Host.IP), status, pingTimeStr, 
			fmt.Sprintf("%.6fs", result.CheckTime.Seconds()), mountStatus)
	}
	
	fmt.Println("=================================================================")
	fmt.Println("SUMMARY:")
	fmt.Printf("  Total hosts configured: %d\n", totalHosts)
	fmt.Printf("  Hosts reachable: %d\n", reachableHosts)
	fmt.Printf("  Hosts mounted: %d\n", mountedHosts)
	
	successRate := 0
	if reachableHosts > 0 {
		successRate = (mountedHosts * 100) / reachableHosts
	}
	fmt.Printf("  Success rate: %d%%\n", successRate)
	fmt.Println()
	
	if mountedHosts > 0 {
		fmt.Println("Active mount points:")
		for _, result := range results {
			if result.Mounted {
				cmd := exec.Command("df", "-h", result.Host.MountPath)
				output, err := cmd.Output()
				if err == nil {
					lines := strings.Split(string(output), "\n")
					if len(lines) > 1 {
						fields := strings.Fields(lines[1])
						if len(fields) >= 5 {
							usage := fmt.Sprintf("%s used: %s (%s)", fields[1], fields[2], fields[4])
							fmt.Printf("  %s -> %s@%s:%d:%s/ [%s]\n", 
								result.Host.MountPath, result.Host.Username, result.Host.IP, result.Host.Port, result.Host.RemoteDir, usage)
						}
					}
				}
			}
		}
	}
	fmt.Println("=================================================================")
	
	// Debug information section
	fmt.Println()
	fmt.Println("==================== DEBUG CONNECTION INFO ====================")
	fmt.Println("Port Configuration:")
	for _, result := range results {
		fmt.Printf("  %-18s Port: %-5d Mount: %-20s Remote: %s\n", 
			fmt.Sprintf("%s@%s", result.Host.Username, result.Host.IP), result.Host.Port, result.Host.MountPath, result.Host.RemoteDir)
	}
	
	fmt.Println()
	fmt.Println("Actual SSHFS Commands Executed:")
	for _, result := range results {
		if result.ExecutedCmd != "" && result.ExecutedCmd != "already_mounted" {
			fmt.Printf("  %s\n", result.ExecutedCmd)
		} else if result.ExecutedCmd == "already_mounted" {
			fmt.Printf("  %s@%s: Already mounted, no command executed\n", result.Host.Username, result.Host.IP)
		} else {
			fmt.Printf("  %s@%s: No command executed (host not reachable)\n", result.Host.Username, result.Host.IP)
		}
	}
	fmt.Println("=================================================================")
	fmt.Printf("Total execution time: %.6fs\n", totalTime.Seconds())
}

func monitorAndMount(hosts []Host) int {
	results := processHostsParallel(hosts)
	mountedCount := 0
	
	for _, result := range results {
		if result.Mounted {
			mountedCount++
		}
	}
	
	if daemonMode {
		logMessage(fmt.Sprintf("Monitoring cycle complete: %d hosts mounted", mountedCount))
	}
	
	return mountedCount
}

func startDaemon() {
	// Check if already running
	if _, err := os.Stat(PID_FILE); err == nil {
		pidData, err := ioutil.ReadFile(PID_FILE)
		if err == nil {
			pid := strings.TrimSpace(string(pidData))
			// Check if process is still running
			if err := exec.Command("kill", "-0", pid).Run(); err == nil {
				fmt.Printf("SSHFS monitor already running (PID: %s)\n", pid)
				os.Exit(1)
			}
			// Remove stale PID file
			os.Remove(PID_FILE)
		}
	}
	
	// Write PID file
	pid := os.Getpid()
	err := ioutil.WriteFile(PID_FILE, []byte(strconv.Itoa(pid)), 0644)
	if err != nil {
		log.Fatalf("Failed to write PID file: %v", err)
	}
	
	// Initialize logging
	if err := initLogging(); err != nil {
		log.Fatalf("Failed to initialize logging: %v", err)
	}
	defer logFile.Close()
	
	daemonMode = true
	logMessage(fmt.Sprintf("SSHFS monitor started in daemon mode (PID: %d)", pid))
	
	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	go func() {
		<-sigChan
		logMessage("Received shutdown signal, cleaning up...")
		os.Remove(PID_FILE)
		logMessage("SSHFS monitor stopped")
		cancel()
	}()
	
	// Load hosts
	hosts, err := loadHosts()
	if err != nil {
		logMessage(fmt.Sprintf("Error loading hosts: %v", err))
		os.Exit(1)
	}
	
	// Main daemon loop
	ticker := time.NewTicker(CHECK_INTERVAL * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			monitorAndMount(hosts)
		}
	}
}

func stopDaemon() {
	if _, err := os.Stat(PID_FILE); err != nil {
		fmt.Println("SSHFS monitor not running")
		return
	}
	
	pidData, err := ioutil.ReadFile(PID_FILE)
	if err != nil {
		fmt.Printf("Error reading PID file: %v\n", err)
		return
	}
	
	pid := strings.TrimSpace(string(pidData))
	
	// Check if process is running
	if err := exec.Command("kill", "-0", pid).Run(); err != nil {
		fmt.Println("SSHFS monitor not running")
		os.Remove(PID_FILE)
		return
	}
	
	// Kill the process
	fmt.Printf("Stopping SSHFS monitor (PID: %s)...\n", pid)
	if err := exec.Command("kill", pid).Run(); err != nil {
		fmt.Printf("Error stopping daemon: %v\n", err)
		return
	}
	
	// Wait for graceful shutdown
	time.Sleep(2 * time.Second)
	
	// Force kill if still running
	if err := exec.Command("kill", "-0", pid).Run(); err == nil {
		exec.Command("kill", "-9", pid).Run()
		fmt.Println("Force killed SSHFS monitor")
	}
	
	os.Remove(PID_FILE)
	fmt.Println("SSHFS monitor stopped")
}

func statusDaemon() {
	if _, err := os.Stat(PID_FILE); err != nil {
		fmt.Println("SSHFS monitor not running")
		os.Exit(1)
	}
	
	pidData, err := ioutil.ReadFile(PID_FILE)
	if err != nil {
		fmt.Printf("Error reading PID file: %v\n", err)
		os.Exit(1)
	}
	
	pid := strings.TrimSpace(string(pidData))
	
	if err := exec.Command("kill", "-0", pid).Run(); err != nil {
		fmt.Println("SSHFS monitor not running (stale PID file)")
		os.Remove(PID_FILE)
		os.Exit(1)
	}
	
	fmt.Printf("SSHFS monitor running (PID: %s)\n", pid)
	fmt.Printf("Log file: %s\n", LOG_FILE)
	fmt.Printf("Check interval: %ds\n", CHECK_INTERVAL)
}

func followLogs() {
	cmd := exec.Command("tail", "-f", LOG_FILE)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func watchMode() {
	fmt.Println("Starting live status monitor (Press Ctrl+C to exit)...")
	
	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	
	go func() {
		<-sigChan
		fmt.Print("\033[?25h") // Show cursor
		os.Exit(0)
	}()
	
	hosts, err := loadHosts()
	if err != nil {
		log.Fatalf("Error loading hosts: %v", err)
	}
	
	// Fast initial load
	fmt.Print("\033[H\033[2J")
	fmt.Println("Loading SSHFS monitor...")
	
	for {
		results := processHostsParallel(hosts)
		printBootstrapStatus(results)
		time.Sleep(3 * time.Second)
	}
}

func dashboardMode() {
	hosts, err := loadHosts()
	if err != nil {
		log.Fatalf("Error loading hosts: %v", err)
	}
	
	results := processHostsParallel(hosts)
	printBootstrapStatus(results)
}

func showUsage() {
	fmt.Println("SSHFS Auto-Mount Monitor (Go)")
	fmt.Println()
	fmt.Println("Usage: ./sshfs-connector {start|stop|restart|status|logs|once|watch|dashboard}")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  start      - Start daemon mode (continuous monitoring)")
	fmt.Println("  stop       - Stop daemon mode")
	fmt.Println("  restart    - Restart daemon mode")
	fmt.Println("  status     - Show daemon status")
	fmt.Println("  logs       - Follow log file")
	fmt.Println("  once       - Run once with full stats")
	fmt.Println("  watch      - Live Bootstrap-style status display (default)")
	fmt.Println("  dashboard  - Single Bootstrap-style status snapshot")
	fmt.Println()
	
	hosts, _ := loadHosts()
	fmt.Println("Configuration:")
	fmt.Printf("  Check interval: %ds\n", CHECK_INTERVAL)
	fmt.Printf("  Log file: %s\n", LOG_FILE)
	fmt.Printf("  PID file: %s\n", PID_FILE)
	fmt.Printf("  Hosts file: %s\n", HOSTS_FILE)
	if len(hosts) > 0 {
		var hostEntries []string
		for _, host := range hosts {
			hostEntries = append(hostEntries, fmt.Sprintf("%s@%s", host.Username, host.IP))
		}
		fmt.Printf("  Hosts (%d): %s\n", len(hosts), strings.Join(hostEntries, ", "))
	} else {
		fmt.Printf("  Hosts: Error loading from %s\n", HOSTS_FILE)
	}
}

func main() {
	if len(os.Args) < 2 {
		watchMode() // Default to watch mode
		return
	}

	command := os.Args[1]
	
	switch command {
	case "start":
		startDaemon()
	case "stop":
		stopDaemon()
	case "restart":
		stopDaemon()
		time.Sleep(1 * time.Second)
		startDaemon()
	case "status":
		statusDaemon()
	case "logs":
		followLogs()
	case "once":
		start := time.Now()
		fmt.Printf("SSHFS Auto-Mount Script (Go) - %s\n", time.Now().Format("Mon Jan 2 15:04:05 MST 2006"))
		fmt.Println("Autodetecting and mounting SSHFS hosts in parallel...")
		fmt.Println()
		
		hosts, err := loadHosts()
		if err != nil {
			log.Fatalf("Error loading hosts: %v", err)
		}
		
		results := processHostsParallel(hosts)
		totalTime := time.Since(start)
		
		printStats(results, totalTime)
	case "watch":
		watchMode()
	case "dashboard":
		dashboardMode()
	default:
		showUsage()
	}
}