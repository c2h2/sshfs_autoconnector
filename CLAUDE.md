# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is an SSHFS auto-connector tool that automatically discovers and mounts remote hosts via SSHFS. The tool provides both one-time execution and daemon mode for continuous monitoring of remote hosts.

## Key Commands

### Running the tool
```bash
# Run once with detailed stats
./connect_sshfs.sh once

# Start daemon mode for continuous monitoring
./connect_sshfs.sh start

# Stop daemon
./connect_sshfs.sh stop

# Check daemon status
./connect_sshfs.sh status

# View live status dashboard
./connect_sshfs.sh watch

# View single status snapshot
./connect_sshfs.sh dashboard

# Follow logs
./connect_sshfs.sh logs
```

### Making the script executable
```bash
chmod +x connect_sshfs.sh
```

## Architecture

### Core Components
- **connect_sshfs.sh**: Main bash script containing all functionality
- **sshfs_hosts.txt**: Configuration file with target host IP addresses (one per line)

### Key Functions
- `load_hosts()`: Reads and validates host configuration from sshfs_hosts.txt:3
- `check_host()`: Performs ping connectivity tests and collects timing stats
- `mount_host()`: Handles SSHFS mounting with error handling and retry logic
- `monitor_and_mount()`: Main monitoring loop that checks and mounts all configured hosts
- `print_bootstrap_status()`: Renders the live dashboard UI with host status and metrics

### Mounting Strategy
- First host mounts to `/root/sshfs`
- Additional hosts mount to `/root/sshfs2`, `/root/sshfs3`, etc.
- Each mount connects to `root@{host}:/root/` on the remote system
- Mount options: `cache=no,attr_timeout=0,entry_timeout=0` for real-time access

### Daemon Operation
- Uses PID file at `/var/run/sshfs-monitor.pid`
- Logs to `/var/log/sshfs-monitor.log`
- Default check interval: 30 seconds
- Automatic cleanup on shutdown signals (SIGTERM, SIGINT)

## Configuration Files

### sshfs_hosts.txt Format
```
# Comments start with #
192.168.26.104
192.168.24.116
192.168.30.119
```

### Key Constants (connect_sshfs.sh:3-10)
- `HOSTS_FILE`: Path to host configuration file
- `MOUNT_BASE`: Base directory for mount points
- `TIMEOUT`: Ping timeout in seconds (default: 3)
- `CHECK_INTERVAL`: Daemon monitoring interval (default: 30s)
- `LOG_FILE`: Daemon log file location
- `PID_FILE`: Process ID file for daemon management

## Dependencies
- `sshfs`: For mounting remote filesystems
- `ping`: For connectivity testing
- `ssh`: For remote command execution (hostname, uptime, MAC address queries)
- `bc`: For floating-point arithmetic in timing calculations
- Standard Unix utilities: `df`, `mountpoint`, `fusermount`

## Development Guidelines
- Always use English in all code and documentation
- Never use Claude or Anthropic names in commit messages
- Keep commit messages concise and descriptive
- Make code changes directly without asking permission
- Open Cursor or any tools without asking - just do it