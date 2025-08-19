#!/bin/bash

HOSTS_FILE="./sshfs_hosts.txt"
MOUNT_BASE="/root"
MOUNT_OPTIONS="cache=no,attr_timeout=0,entry_timeout=0"
TIMEOUT=3
CHECK_INTERVAL=30
LOG_FILE="/var/log/sshfs-monitor.log"
PID_FILE="/var/run/sshfs-monitor.pid"
DAEMON_MODE=false

declare -a HOSTS
declare -a MOUNT_PATHS
declare -a PORTS
declare -a REMOTE_DIRS
declare -A HOST_STATS
declare -A MOUNT_TIMES
declare -A EXECUTED_COMMANDS

load_hosts() {
    if [ ! -f "$HOSTS_FILE" ]; then
        echo "Error: Hosts file $HOSTS_FILE not found!"
        echo "Please create $HOSTS_FILE with hostname, mount_path, port, and remote_dir per line."
        exit 1
    fi
    
    HOSTS=()
    MOUNT_PATHS=()
    PORTS=()
    REMOTE_DIRS=()
    while IFS= read -r line || [ -n "$line" ]; do
        line=$(echo "$line" | xargs)
        if [ -n "$line" ] && [[ ! "$line" =~ ^# ]]; then
            read -r host mount_path port remote_dir <<< "$line"
            if [ -n "$host" ]; then
                HOSTS+=("$host")
                
                # Handle mount path
                if [ -n "$mount_path" ]; then
                    # Handle relative paths
                    if [[ "$mount_path" != /* ]]; then
                        mount_path="$MOUNT_BASE/$mount_path"
                    fi
                    MOUNT_PATHS+=("$mount_path")
                else
                    # Default mount path if not specified
                    if [ ${#HOSTS[@]} -eq 1 ]; then
                        MOUNT_PATHS+=("$MOUNT_BASE/sshfs")
                    else
                        MOUNT_PATHS+=("$MOUNT_BASE/sshfs${#HOSTS[@]}")
                    fi
                fi
                
                # Handle port (default to 22)
                if [ -n "$port" ] && [[ "$port" =~ ^[0-9]+$ ]]; then
                    PORTS+=("$port")
                else
                    PORTS+=("22")
                fi
                
                # Handle remote directory (default to /root)
                if [ -n "$remote_dir" ]; then
                    REMOTE_DIRS+=("$remote_dir")
                else
                    REMOTE_DIRS+=("/root")
                fi
            fi
        fi
    done < "$HOSTS_FILE"
    
    if [ ${#HOSTS[@]} -eq 0 ]; then
        echo "Error: No hosts found in $HOSTS_FILE"
        exit 1
    fi
}

log() {
    local message="$1"
    local timestamp=$(date '+%Y-%m-%d %H:%M:%S')
    echo "[$timestamp] $message" | tee -a "$LOG_FILE"
}

cleanup() {
    log "Received shutdown signal, cleaning up..."
    printf "\033[?25h"  # Show cursor
    if [ -f "$PID_FILE" ]; then
        rm -f "$PID_FILE"
    fi
    log "SSHFS monitor stopped"
    exit 0
}

start_daemon() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            echo "SSHFS monitor already running (PID: $pid)"
            exit 1
        else
            rm -f "$PID_FILE"
        fi
    fi
    
    echo $$ > "$PID_FILE"
    log "SSHFS monitor started in daemon mode (PID: $$)"
    
    trap cleanup SIGTERM SIGINT
    
    while true; do
        monitor_and_mount
        sleep "$CHECK_INTERVAL"
    done
}

stop_daemon() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid"
            echo "Stopping SSHFS monitor (PID: $pid)..."
            sleep 2
            if kill -0 "$pid" 2>/dev/null; then
                kill -9 "$pid"
                echo "Force killed SSHFS monitor"
            fi
            rm -f "$PID_FILE"
            echo "SSHFS monitor stopped"
        else
            echo "SSHFS monitor not running"
            rm -f "$PID_FILE"
        fi
    else
        echo "SSHFS monitor not running"
    fi
}

status_daemon() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            echo "SSHFS monitor running (PID: $pid)"
            echo "Log file: $LOG_FILE"
            echo "Check interval: ${CHECK_INTERVAL}s"
            return 0
        else
            echo "SSHFS monitor not running (stale PID file)"
            rm -f "$PID_FILE"
            return 1
        fi
    else
        echo "SSHFS monitor not running"
        return 1
    fi
}

get_ping_time() {
    local host=$1
    local ping_result=$(ping -c 1 -W $TIMEOUT "$host" 2>/dev/null | grep "time=")
    if [ $? -eq 0 ]; then
        echo "$ping_result" | sed -n 's/.*time=\([0-9.]*\).*/\1/p'
    else
        echo "N/A"
    fi
}

check_host() {
    local host=$1
    local start_time=$(date +%s.%N)
    ping -c 1 -W $TIMEOUT "$host" &>/dev/null
    local result=$?
    local end_time=$(date +%s.%N)
    local ping_time=$(get_ping_time "$host")
    
    HOST_STATS["$host"]="$result,$ping_time,$(echo "$end_time - $start_time" | bc -l)"
    return $result
}

clear_stale_endpoint() {
    local mount_point=$1
    local host=$2
    
    if [ ! -d "$mount_point" ]; then
        return 0
    fi
    
    # Check if it's a stale mount (directory exists but can't access it)
    if ! ls "$mount_point" >/dev/null 2>&1; then
        echo "Detected stale SSHFS endpoint at $mount_point, clearing..."
        
        # Try multiple cleanup methods
        fusermount -u "$mount_point" 2>/dev/null
        if [ $? -ne 0 ]; then
            umount "$mount_point" 2>/dev/null
        fi
        if [ $? -ne 0 ]; then
            umount -l "$mount_point" 2>/dev/null  # Lazy unmount
        fi
        
        # Wait a moment for cleanup to complete
        sleep 1
        
        # Verify cleanup worked
        if ls "$mount_point" >/dev/null 2>&1; then
            echo "Successfully cleared stale endpoint: $mount_point"
            return 0
        else
            echo "Warning: Could not fully clear stale endpoint: $mount_point"
            return 1
        fi
    fi
    
    return 0
}

mount_host() {
    local host=$1
    local mount_point=$2
    local port=$3
    local remote_dir=$4
    
    if mountpoint -q "$mount_point" 2>/dev/null; then
        if ! ls "$mount_point" >/dev/null 2>&1; then
            log "Mount point $mount_point appears stale, unmounting..."
            fusermount -u "$mount_point" 2>/dev/null || umount "$mount_point" 2>/dev/null
        else
            if [ "$DAEMON_MODE" = true ]; then
                log "Mount verified: $mount_point"
            else
                echo "Already mounted: $mount_point"
            fi
            MOUNT_TIMES["$host"]="already_mounted"
            return 0
        fi
    fi
    
    # Clear any stale endpoints first
    clear_stale_endpoint "$mount_point" "$host"
    
    # Create mount point directory only if it doesn't exist
    if [ ! -d "$mount_point" ]; then
        mkdir -p "$mount_point"
    fi
    
    local start_time=$(date +%s.%N)
    local sshfs_cmd="sshfs root@$host:$remote_dir/ $mount_point -o $MOUNT_OPTIONS,port=$port"
    EXECUTED_COMMANDS["$host"]="$sshfs_cmd"
    if sshfs "root@$host:$remote_dir/" "$mount_point" -o "$MOUNT_OPTIONS,port=$port" 2>/dev/null; then
        local end_time=$(date +%s.%N)
        local mount_time=$(echo "$end_time - $start_time" | bc -l)
        MOUNT_TIMES["$host"]="$mount_time"
        local msg="Successfully mounted: $host:$port -> $mount_point (${mount_time}s)"
        if [ "$DAEMON_MODE" = true ]; then
            log "$msg"
        else
            echo "$msg"
        fi
        return 0
    else
        local end_time=$(date +%s.%N)
        local mount_time=$(echo "$end_time - $start_time" | bc -l)
        MOUNT_TIMES["$host"]="failed_${mount_time}"
        local msg="Failed to mount: $host:$port (${mount_time}s)"
        if [ "$DAEMON_MODE" = true ]; then
            log "$msg"
        else
            echo "$msg"
        fi
        # Don't remove directory on mount failure - user might have files there
        return 1
    fi
}

get_remote_info() {
    local host=$1
    local mount_point=$2
    local info_type=$3
    local port=$4
    
    if mountpoint -q "$mount_point" 2>/dev/null; then
        case "$info_type" in
            "uptime")
                local result=$(ssh -p "$port" -o ConnectTimeout=2 -o StrictHostKeyChecking=no "root@$host" "uptime | sed 's/.*up \\([^,]*\\).*/\\1/' | xargs" 2>/dev/null)
                ;;
            "hostname")
                local result=$(ssh -p "$port" -o ConnectTimeout=2 -o StrictHostKeyChecking=no "root@$host" "hostname" 2>/dev/null)
                ;;
            "mac")
                local result=$(ssh -p "$port" -o ConnectTimeout=2 -o StrictHostKeyChecking=no "root@$host" "cat /sys/class/net/eth0/address 2>/dev/null || ip link show eth0 2>/dev/null | grep -o '[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]' | head -1" 2>/dev/null)
                ;;
        esac
        
        if [ $? -eq 0 ] && [ -n "$result" ]; then
            echo "$result"
        else
            echo "N/A"
        fi
    else
        echo "N/A"
    fi
}

get_local_info() {
    local info_type=$1
    
    case "$info_type" in
        "hostname")
            hostname 2>/dev/null || echo "N/A"
            ;;
        "mac")
            cat /sys/class/net/eth0/address 2>/dev/null || ip link show eth0 2>/dev/null | grep -o '[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]' | head -1 2>/dev/null || echo "N/A"
            ;;
    esac
}

get_status_badge() {
    local host=$1
    local mount_point=$2
    local stats="${HOST_STATS[$host]}"
    IFS=',' read -r ping_result ping_time check_time <<< "$stats"
    
    if [ "$ping_result" -eq 0 ]; then
        local mount_info="${MOUNT_TIMES[$host]}"
        if [[ "$mount_info" == "already_mounted" ]] || [[ "$mount_info" =~ ^[0-9] ]]; then
            if mountpoint -q "$mount_point" 2>/dev/null; then
                echo -e "\033[42m\033[30m ONLINE  \033[0m"  # Green (7 chars)
            else
                echo -e "\033[43m\033[30m STALE   \033[0m"  # Yellow (7 chars)
            fi
        else
            echo -e "\033[43m\033[30m CONN-ERR\033[0m"     # Yellow (8 chars)
        fi
    else
        echo -e "\033[41m\033[37m OFFLINE \033[0m"        # Red (7 chars)
    fi
}

print_bootstrap_status() {
    local output=""
    local uptime_info=$(uptime | sed 's/.*up \([^,]*\).*/\1/' | xargs)
    local local_hostname=$(get_local_info "hostname")
    local local_mac=$(get_local_info "mac")
    
    # Build output buffer to minimize flicker
    output+="\033[?25l"      # Hide cursor
    output+="\033[H\033[2J"  # Clear screen and move cursor to top
    output+="\033[1;36m╔══════════════════════════════════════════════════════════════╗\033[0m\n"
    output+="\033[1;36m║                    SSHFS STATUS MONITOR                     ║\033[0m\n"
    
    # Calculate padding for local info line (62 chars inside box)
    local local_info="  Local: $local_hostname | Uptime: $uptime_info"
    local local_padding=$((62 - ${#local_info}))
    output+="\033[1;36m║$local_info$(printf "%*s" "$local_padding" "")║\033[0m\n"
    
    # Calculate padding for MAC line (62 chars inside box)
    local mac_info="  MAC: $local_mac"
    local mac_padding=$((62 - ${#mac_info}))
    output+="\033[1;36m║$mac_info$(printf "%*s" "$mac_padding" "")║\033[0m\n"
    
    output+="\033[1;36m╚══════════════════════════════════════════════════════════════╝\033[0m\n"
    output+="\n"
    
    local total_hosts=0
    local online_hosts=0
    
    for i in "${!HOSTS[@]}"; do
        local host="${HOSTS[$i]}"
        local mount_point="${MOUNT_PATHS[$i]}"
        local port="${PORTS[$i]}"
        local remote_dir="${REMOTE_DIRS[$i]}"
        
        ((total_hosts++))
        local badge=$(get_status_badge "$host" "$mount_point")
        local stats="${HOST_STATS[$host]}"
        IFS=',' read -r ping_result ping_time check_time <<< "$stats"
        
        local host_label="Host $((i+1))"
        local ping_display="${ping_time:-N/A}ms"
        
        local remote_uptime=$(get_remote_info "$host" "$mount_point" "uptime" "$port")
        local remote_hostname=$(get_remote_info "$host" "$mount_point" "hostname" "$port")
        local remote_mac=$(get_remote_info "$host" "$mount_point" "mac" "$port")
        
        if [ "$ping_result" -eq 0 ]; then
            ((online_hosts++))
            local mount_info="${MOUNT_TIMES[$host]}"
            if [[ "$mount_info" == "already_mounted" ]] || [[ "$mount_info" =~ ^[0-9] ]]; then
                if mountpoint -q "$mount_point" 2>/dev/null; then
                    local usage=$(df -h "$mount_point" 2>/dev/null | tail -1 | awk '{print $5}' | tr -d '%')
                    local usage_bar=""
                    local bar_length=$((usage / 10))
                    for ((j=0; j<10; j++)); do
                        if [ $j -lt $bar_length ]; then
                            usage_bar+="█"
                        else
                            usage_bar+="░"
                        fi
                    done
                    output+="  $badge $host_label ($host) | Host: $remote_hostname | Ping: $ping_display | Mount: $mount_point [$usage_bar ${usage}%] | Up: $remote_uptime\n"
                    output+="    \033[2m└─ MAC: $remote_mac\033[0m\n"
                else
                    output+="  $badge $host_label ($host) | Host: $remote_hostname | Ping: $ping_display | Mount: $mount_point (stale) | Up: $remote_uptime\n"
                    output+="    \033[2m└─ MAC: $remote_mac\033[0m\n"
                fi
            else
                output+="  $badge $host_label ($host) | Host: $remote_hostname | Ping: $ping_display | Mount: Failed to connect | Up: $remote_uptime\n"
                output+="    \033[2m└─ MAC: $remote_mac\033[0m\n"
            fi
        else
            output+="  $badge $host_label ($host) | Host: N/A | Ping: N/A | Mount: Not available | Up: N/A\n"
            output+="    \033[2m└─ MAC: N/A\033[0m\n"
        fi
    done
    
    output+="\033[1;34m┌─ SUMMARY ─────────────────────────────────────────────────────┐\033[0m\n"
    
    # Calculate padding for summary line (62 chars inside box)
    local success_rate=$(( total_hosts > 0 ? (online_hosts * 100) / total_hosts : 0 ))
    local summary_info=" Total: $total_hosts hosts │ Online: $online_hosts hosts │ Success: $success_rate%"
    local summary_padding=$((62 - ${#summary_info}))
    output+="\033[1;34m│\033[0m$summary_info$(printf "%*s" "$summary_padding" "")\033[1;34m│\033[0m\n"
    
    output+="\033[1;34m└───────────────────────────────────────────────────────────────┘\033[0m\n"
    output+="\n"
    output+="\033[2mLast updated: $(date '+%Y-%m-%d %H:%M:%S')\033[0m\n"
    
    if [ "$DAEMON_MODE" = true ]; then
        output+="\033[2mNext check in: ${CHECK_INTERVAL}s | Press Ctrl+C to stop\033[0m\n"
    fi
    
    output+="\033[?25h"      # Show cursor
    
    # Output everything at once to minimize flicker
    printf "%b" "$output"
}

print_stats() {
    echo
    echo "==================== SSHFS CONNECTION STATS ===================="
    printf "%-18s %-12s %-12s %-15s %-15s\n" "HOST" "STATUS" "PING (ms)" "PING TIME" "MOUNT TIME"
    echo "----------------------------------------------------------------"
    
    local total_hosts=0
    local reachable_hosts=0
    local mounted_hosts=0
    
    for host in "${HOSTS[@]}"; do
        ((total_hosts++))
        local stats="${HOST_STATS[$host]}"
        IFS=',' read -r ping_result ping_time check_time <<< "$stats"
        
        local status="UNREACHABLE"
        local mount_status="N/A"
        
        if [ "$ping_result" -eq 0 ]; then
            status="REACHABLE"
            ((reachable_hosts++))
            
            local mount_info="${MOUNT_TIMES[$host]}"
            if [[ "$mount_info" == "already_mounted" ]]; then
                mount_status="ALREADY MOUNTED"
                ((mounted_hosts++))
            elif [[ "$mount_info" =~ ^failed_ ]]; then
                mount_status="FAILED (${mount_info#failed_}s)"
            elif [[ "$mount_info" =~ ^[0-9] ]]; then
                mount_status="SUCCESS (${mount_info}s)"
                ((mounted_hosts++))
            fi
        fi
        
        printf "%-18s %-12s %-12s %-15s %-15s\n" \
            "$host" "$status" "${ping_time:-N/A}" "${check_time}s" "$mount_status"
    done
    
    echo "================================================================="
    echo "SUMMARY:"
    echo "  Total hosts configured: $total_hosts"
    echo "  Hosts reachable: $reachable_hosts"
    echo "  Hosts mounted: $mounted_hosts"
    echo "  Success rate: $(( reachable_hosts > 0 ? (mounted_hosts * 100) / reachable_hosts : 0 ))%"
    echo
    
    if [ $mounted_hosts -gt 0 ]; then
        echo "Active mount points:"
        for i in "${!HOSTS[@]}"; do
            local host="${HOSTS[$i]}"
            local mount_point="${MOUNT_PATHS[$i]}"
            local port="${PORTS[$i]}"
            local remote_dir="${REMOTE_DIRS[$i]}"
            
            if mountpoint -q "$mount_point" 2>/dev/null; then
                local usage=$(df -h "$mount_point" 2>/dev/null | tail -1 | awk '{print $2 " used: " $3 " (" $5 ")"}')
                echo "  $mount_point -> root@$host:$port:$remote_dir/ [$usage]"
            fi
        done
    fi
    echo "================================================================="
    
    # Debug information section
    echo
    echo "==================== DEBUG CONNECTION INFO ===================="
    echo "Port Configuration:"
    for i in "${!HOSTS[@]}"; do
        local host="${HOSTS[$i]}"
        local mount_point="${MOUNT_PATHS[$i]}"
        local port="${PORTS[$i]}"
        local remote_dir="${REMOTE_DIRS[$i]}"
        printf "  %-18s Port: %-5s Mount: %-20s Remote: %s\n" "$host" "$port" "$mount_point" "$remote_dir"
    done
    
    echo
    echo "Actual SSHFS Commands Executed:"
    for i in "${!HOSTS[@]}"; do
        local host="${HOSTS[$i]}"
        local executed_cmd="${EXECUTED_COMMANDS[$host]}"
        if [ -n "$executed_cmd" ]; then
            echo "  $executed_cmd"
        else
            echo "  $host: No command executed (host not reachable)"
        fi
    done
    echo "================================================================="
}

monitor_and_mount() {
    load_hosts
    local mounted_count=0
    
    for i in "${!HOSTS[@]}"; do
        local host="${HOSTS[$i]}"
        local mount_point="${MOUNT_PATHS[$i]}"
        local port="${PORTS[$i]}"
        local remote_dir="${REMOTE_DIRS[$i]}"
        
        if check_host "$host"; then
            local stats="${HOST_STATS[$host]}"
            IFS=',' read -r ping_result ping_time check_time <<< "$stats"
            
            if [ "$DAEMON_MODE" = true ]; then
                log "Host $host reachable (ping: ${ping_time}ms)"
            else
                echo "[$((i+1))/${#HOSTS[@]}] Checking host: $host"
                echo "  ✓ Host reachable (ping: ${ping_time}ms)"
            fi
            
            if mount_host "$host" "$mount_point" "$port" "$remote_dir"; then
                ((mounted_count++))
            fi
        else
            if [ "$DAEMON_MODE" = true ]; then
                log "Host $host not reachable"
            else
                echo "[$((i+1))/${#HOSTS[@]}] Checking host: $host"
                echo "  ✗ Host not reachable"
            fi
        fi
        
        if [ "$DAEMON_MODE" != true ]; then
            echo
        fi
    done
    
    if [ "$DAEMON_MODE" = true ]; then
        log "Monitoring cycle complete: $mounted_count hosts mounted"
    fi
    
    return $mounted_count
}

main() {
    case "${1:-}" in
        start)
            DAEMON_MODE=true
            start_daemon
            ;;
        stop)
            stop_daemon
            ;;
        restart)
            stop_daemon
            sleep 1
            DAEMON_MODE=true
            start_daemon
            ;;
        status)
            status_daemon
            ;;
        logs)
            tail -f "$LOG_FILE"
            ;;
        once)
            local script_start=$(date +%s.%N)
            echo "SSHFS Auto-Mount Script - $(date)"
            echo "Autodetecting and mounting SSHFS hosts..."
            echo
            
            monitor_and_mount
            
            local script_end=$(date +%s.%N)
            local total_time=$(echo "$script_end - $script_start" | bc -l)
            
            print_stats
            echo "Total execution time: ${total_time}s"
            ;;
        watch)
            echo "Starting live status monitor (Press Ctrl+C to exit)..."
            trap 'printf "\033[?25h"; exit 0' SIGINT SIGTERM
            # Fast initial load - show loading screen first
            load_hosts
            printf "\033[H\033[2J"
            echo "Loading SSHFS monitor..."
            while true; do
                monitor_and_mount >/dev/null 2>&1
                print_bootstrap_status
                sleep 3
            done
            ;;
        dashboard)
            monitor_and_mount >/dev/null 2>&1
            print_bootstrap_status
            ;;
        "")
            # Default action when no arguments provided - run watch mode
            echo "Starting live status monitor (Press Ctrl+C to exit)..."
            trap 'printf "\033[?25h"; exit 0' SIGINT SIGTERM
            # Fast initial load - show loading screen first
            load_hosts
            printf "\033[H\033[2J"
            echo "Loading SSHFS monitor..."
            while true; do
                monitor_and_mount >/dev/null 2>&1
                print_bootstrap_status
                sleep 3
            done
            ;;
        *)
            echo "SSHFS Auto-Mount Monitor"
            echo
            echo "Usage: $0 {start|stop|restart|status|logs|once|watch|dashboard}"
            echo
            echo "Commands:"
            echo "  start      - Start daemon mode (continuous monitoring)"
            echo "  stop       - Stop daemon mode"
            echo "  restart    - Restart daemon mode"
            echo "  status     - Show daemon status"
            echo "  logs       - Follow log file"
            echo "  once       - Run once with full stats"
            echo "  watch      - Live Bootstrap-style status display (default)"
            echo "  dashboard  - Single Bootstrap-style status snapshot"
            echo
            load_hosts >/dev/null 2>&1 || true
            echo "Configuration:"
            echo "  Check interval: ${CHECK_INTERVAL}s"
            echo "  Log file: $LOG_FILE"
            echo "  PID file: $PID_FILE"
            echo "  Hosts file: $HOSTS_FILE"
            if [ ${#HOSTS[@]} -gt 0 ]; then
                echo "  Hosts (${#HOSTS[@]}): ${HOSTS[*]}"
            else
                echo "  Hosts: Error loading from $HOSTS_FILE"
            fi
            ;;
    esac
}

main "$@"
