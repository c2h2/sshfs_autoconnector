# SSHFS Auto Connector

Automatically discover and mount remote Linux hosts via SSHFS with real-time monitoring.

## Quick Start

1. Configure target hosts in `sshfs_hosts.txt`:
   ```
   192.168.1.100
   192.168.1.101
   ```

2. Run the tool:
   ```bash
   chmod +x connect_sshfs.sh
   ./connect_sshfs.sh once    # Run once
   ./connect_sshfs.sh start   # Start daemon
   ./connect_sshfs.sh watch   # Live dashboard
   ```

## Features

- **Auto-discovery**: Ping hosts and mount available ones
- **Daemon mode**: Continuous monitoring with 30s intervals  
- **Live dashboard**: Bootstrap-style status display
- **Mount management**: Automatic retry and cleanup
- **Remote info**: Displays hostname, uptime, disk usage, MAC addresses

## Commands

| Command | Description |
|---------|-------------|
| `once` | Single run with detailed stats |
| `start/stop` | Daemon mode control |
| `watch` | Live status monitor |
| `dashboard` | Status snapshot |
| `logs` | Follow daemon logs |

## Mount Points

- First host: `/root/sshfs`
- Additional: `/root/sshfs2`, `/root/sshfs3`, etc.
- Remote path: `root@{host}:/root/`

## Requirements

- `sshfs`, `ssh`, `ping`, `bc`
- SSH key authentication to target hosts
- Root access on target systems
