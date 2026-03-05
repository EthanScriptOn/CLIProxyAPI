#!/bin/bash

# Proxy Server Linux Installer
# Linux-specific script that installs, upgrades, and manages the proxy server
# Reads the release package from PACKAGE_DIR and installs it locally

set -euo pipefail

# Configuration
INSTALL_DIR="$HOME/cliproxyapi"
PACKAGE_DIR="${CLI_PROXY_DIR:-/root/cliproxyapi}"   # 包所在目录，由 claude-proxy-setup.sh 传入或默认
SCRIPT_NAME="cliproxyapi-installer"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_step() {
    echo -e "${CYAN}[STEP]${NC} $1"
}

# Display authentication information for first-time setup
show_authentication_info() {
    echo
    echo -e "${YELLOW}🔐 IMPORTANT: Authentication Setup Required${NC}"
    echo
    echo -e "${YELLOW}Authentication Commands:${NC}"
    echo
    echo -e "${GREEN}Gemini (Google):${NC}"
    echo "  ./cli-proxy-api --login"
    echo "  ./cli-proxy-api --login --project_id <your_project_id>"
    echo "  (OAuth callback on port 8085)"
    echo
    echo -e "${GREEN}OpenAI (Codex/GPT):${NC}"
    echo "  ./cli-proxy-api --codex-login"
    echo "  (OAuth callback on port 1455)"
    echo
    echo -e "${GREEN}Claude (Anthropic):${NC}"
    echo "  ./cli-proxy-api --claude-login"
    echo "  (OAuth callback on port 54545)"
    echo
    echo -e "${GREEN}Qwen:${NC}"
    echo "  ./cli-proxy-api --qwen-login"
    echo "  (Uses OAuth device flow)"
    echo
    echo -e "${GREEN}iFlow:${NC}"
    echo "  ./cli-proxy-api --iflow-login"
    echo "  (OAuth callback on port 11451)"
    echo
    echo -e "${YELLOW}💡 Tip: Add --no-browser to any login command to print URL instead"
    echo "     of automatically opening a browser."
    echo
}

# Generate OpenAI-format API key
generate_api_key() {
    local prefix="sk-"
    local chars="abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    local key=""
    for i in {1..45}; do
        key="${key}${chars:$((RANDOM % ${#chars})):1}"
    done
    echo "${prefix}${key}"
}

# Check if API keys are configured
check_api_keys() {
    local config_file="${INSTALL_DIR}/config.yaml"
    if [[ ! -f "$config_file" ]]; then
        return 1
    fi
    if grep -q '"your-api-key-1"' "$config_file" || grep -q '"your-api-key-2"' "$config_file"; then
        return 1
    fi
    if grep -A 10 "^api-keys:" "$config_file" | grep -v "^#" | grep -v "^api-keys:" | grep -q '"sk-[^"]*"'; then
        return 0
    fi
    return 1
}

# Show API key setup guidance
show_api_key_setup() {
    echo
    echo -e "${YELLOW}🔑 IMPORTANT: API Keys Required Before First Run${NC}"
    echo
    echo -e "${GREEN}1. Edit the configuration file:${NC}"
    echo -e "   ${CYAN}nano ${INSTALL_DIR}/config.yaml${NC}"
    echo
    echo -e "${GREEN}2. Find the 'api-keys' section and replace placeholder keys${NC}"
    echo
}

# Show quick start guide
show_quick_start() {
    local install_dir="$1"
    echo
    echo -e "${GREEN}🚀 Quick Start Guide:${NC}"
    echo -e "${BLUE}1. Navigate to install directory:${NC}"
    echo -e "   ${CYAN}cd $install_dir${NC}"
    echo
    if ! check_api_keys; then
        show_api_key_setup
        echo -e "${BLUE}2. Set up authentication (choose one or more):${NC}"
    else
        echo -e "${BLUE}2. Set up authentication (choose one or more):${NC}"
    fi
    echo -e "   ${CYAN}./cli-proxy-api --login${NC}           # For Gemini"
    echo -e "   ${CYAN}./cli-proxy-api --codex-login${NC}     # For OpenAI"
    echo -e "   ${CYAN}./cli-proxy-api --claude-login${NC}    # For Claude"
    echo -e "   ${CYAN}./cli-proxy-api --qwen-login${NC}      # For Qwen"
    echo -e "   ${CYAN}./cli-proxy-api --iflow-login${NC}     # For iFlow"
    echo
    echo -e "${BLUE}3. Start the service:${NC}"
    echo -e "   ${CYAN}./cli-proxy-api${NC}"
    echo
    echo -e "${BLUE}4. Or run as a systemd service:${NC}"
    echo -e "   ${CYAN}systemctl --user enable cliproxyapi.service${NC}"
    echo -e "   ${CYAN}systemctl --user start cliproxyapi.service${NC}"
    echo -e "   ${CYAN}systemctl --user status cliproxyapi.service${NC}"
    echo
}

# Detect Linux architecture
detect_linux_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "linux_amd64" ;;
        arm64|aarch64) echo "linux_arm64" ;;
        *)
            log_error "Unsupported architecture: $(uname -m). Only x86_64 and arm64 are supported."
            exit 1
            ;;
    esac
}

# Check if required tools are available
check_dependencies() {
    local missing_tools=()
    if ! command -v tar >/dev/null 2>&1; then
        missing_tools+=("tar")
    fi
    if [[ ${#missing_tools[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "On Ubuntu/Debian: sudo apt-get install tar"
        log_info "On CentOS/RHEL: sudo yum install tar"
        exit 1
    fi
}

# Find the release package in PACKAGE_DIR
find_package() {
    local arch="$1"
    # 优先匹配带架构后缀的包，再匹配任意 tar.gz
    local pkg
    pkg=$(find "$PACKAGE_DIR" -maxdepth 1 -name "*${arch}*.tar.gz" | sort | tail -1)
    if [[ -z "$pkg" ]]; then
        pkg=$(find "$PACKAGE_DIR" -maxdepth 1 -name "*.tar.gz" | sort | tail -1)
    fi
    if [[ -z "$pkg" ]]; then
        log_error "No .tar.gz package found in $PACKAGE_DIR"
        log_info "Please place the release package in $PACKAGE_DIR before running this script."
        exit 1
    fi
    echo "$pkg"
}

# Extract version from package filename or version.txt inside archive
extract_version_from_package() {
    local pkg="$1"
    # Try to get version from filename (e.g. server_v1.2.3_linux_amd64.tar.gz)
    local version
    version=$(basename "$pkg" | grep -oE 'v[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1)
    if [[ -z "$version" ]]; then
        # Fallback: use date as version
        version="$(date +%Y%m%d)"
    fi
    echo "$version"
}

# Check if proxy server is already installed
is_installed() {
    [[ -f "${INSTALL_DIR}/version.txt" ]]
}

# Get currently installed version
get_current_version() {
    if is_installed; then
        cat "${INSTALL_DIR}/version.txt" 2>/dev/null || echo "unknown"
    else
        echo "none"
    fi
}

# Backup existing configuration
backup_config() {
    local config="${INSTALL_DIR}/config.yaml"
    if [[ -f "$config" ]]; then
        local backup_dir="${INSTALL_DIR}/config_backup"
        mkdir -p "$backup_dir"
        local timestamp
        timestamp=$(date +"%Y%m%d_%H%M%S")
        local backup_file="${backup_dir}/config_${timestamp}.yaml"
        cp "$config" "$backup_file"
        log_info "Configuration backed up to: $backup_file"
        echo "$backup_file"
    else
        echo ""
    fi
}

# Check if systemd service is running
is_service_running() {
    systemctl --user is-active --quiet cliproxyapi.service 2>/dev/null
}

# Check if any proxy processes are running
is_proxy_running() {
    pgrep -f "cli-proxy-api" >/dev/null 2>&1
}

# Stop any running proxy processes
stop_proxy_processes() {
    local pids
    pids=$(pgrep -f "cli-proxy-api" 2>/dev/null || true)
    if [[ -n "$pids" ]]; then
        log_info "Stopping running proxy processes..."
        echo "$pids" | while read -r pid; do
            [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
        done
        sleep 2
        local remaining
        remaining=$(pgrep -f "cli-proxy-api" 2>/dev/null || true)
        if [[ -n "$remaining" ]]; then
            log_warning "Force killing remaining processes..."
            echo "$remaining" | while read -r pid; do
                [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null || true
            done
            sleep 1
        fi
        log_success "All proxy processes stopped"
    else
        log_info "No proxy processes are running"
    fi
}

# Stop systemd service
stop_service() {
    if is_service_running; then
        log_info "Stopping proxy service..."
        systemctl --user stop cliproxyapi.service
        log_success "Service stopped"
    fi
}

# Start systemd service
start_service() {
    log_info "Starting proxy service..."
    systemctl --user start cliproxyapi.service
    sleep 2
    if is_service_running; then
        log_success "Service started successfully"
    else
        log_warning "Service may not have started. Check: systemctl --user status cliproxyapi.service"
    fi
}

# Restart systemd service
restart_service() {
    log_info "Restarting proxy service..."
    systemctl --user restart cliproxyapi.service
    sleep 2
    if is_service_running; then
        log_success "Service restarted successfully"
    else
        log_warning "Service may not have started. Check: systemctl --user status cliproxyapi.service"
    fi
}

# Create systemd service file
create_systemd_service() {
    local install_dir="$1"
    local service_file="${install_dir}/cliproxyapi.service"
    local systemd_dir="$HOME/.config/systemd/user"
    local systemd_service_file="${systemd_dir}/cliproxyapi.service"

    log_info "Creating systemd service file..."
    mkdir -p "$systemd_dir"

    cat > "$service_file" << EOF
[Unit]
Description=Proxy API Service
After=network.target

[Service]
Type=simple
WorkingDirectory=$install_dir
ExecStart=$install_dir/cli-proxy-api
Restart=always
RestartSec=10
Environment=HOME=$HOME

[Install]
WantedBy=default.target
EOF

    cp "$service_file" "$systemd_service_file"
    systemctl --user daemon-reload || log_warning "Could not reload systemd daemon"

    log_success "Systemd service file created: $systemd_service_file"
}

# Setup configuration
setup_config() {
    local version_dir="$1"
    local backup_file="$2"

    log_info "Setting up configuration..."

    local config="${INSTALL_DIR}/config.yaml"
    local example_config="${version_dir}/config.example.yaml"
    local executable="${version_dir}/cli-proxy-api"

    # Copy executable to main directory
    if [[ -f "$executable" ]]; then
        cp "$executable" "${INSTALL_DIR}/cli-proxy-api"
        chmod +x "${INSTALL_DIR}/cli-proxy-api"
        log_success "Copied executable to ${INSTALL_DIR}/cli-proxy-api"
    fi

    # Copy static files
    if [[ -d "${version_dir}/static" ]]; then
        cp -r "${version_dir}/static" "${INSTALL_DIR}/"
        log_success "Copied static files to ${INSTALL_DIR}/static"
    fi

    # Restore backup if upgrading
    if [[ -n "$backup_file" && -f "$backup_file" ]]; then
        cp "$backup_file" "$config"
        log_success "Restored configuration from backup"
        return
    fi

    # Preserve existing config
    if [[ -f "$config" ]]; then
        log_success "Preserved existing configuration (config.yaml)"
        return
    fi

    # Create from example
    if [[ -f "$example_config" ]]; then
        cp "$example_config" "$config"
        local key1 key2
        key1=$(generate_api_key)
        key2=$(generate_api_key)
        sed -i "s/\"your-api-key-1\"/\"$key1\"/g" "$config"
        sed -i "s/\"your-api-key-2\"/\"$key2\"/g" "$config"
        log_success "Created config.yaml from example with generated API keys"
        log_info "API keys: $key1, $key2"
    else
        log_warning "config.example.yaml not found, please create config.yaml manually"
    fi
}

# Extract tar.gz archive
extract_archive() {
    local archive="$1"
    local dest_dir="$2"
    log_info "Extracting archive to $dest_dir..."
    mkdir -p "$dest_dir"
    tar -xzf "$archive" -C "$dest_dir"
    log_success "Extraction completed"
}

# Write version file
write_version_file() {
    local install_dir="$1"
    local version="$2"
    echo "$version" > "${install_dir}/version.txt"
    log_success "Version $version written to version.txt"
}

# Clean up old versions (keep last 2)
cleanup_old_versions() {
    local current_version="$1"
    if [[ ! -d "$INSTALL_DIR" ]]; then return; fi
    log_info "Cleaning up old versions..."
    local old_versions
    old_versions=$(find "$INSTALL_DIR" -maxdepth 1 -type d -name "*.*" -printf "%f\n" 2>/dev/null | sort -V | head -n -2 || true)
    if [[ -n "$old_versions" ]]; then
        echo "$old_versions" | while read -r version; do
            if [[ "$version" != "$current_version" && -n "$version" ]]; then
                rm -rf "${INSTALL_DIR}/${version}"
                log_info "Removed old version: $version"
            fi
        done
    fi
}

# Main installation function
install_proxy() {
    local current_version
    current_version=$(get_current_version)
    local is_upgrade=false
    local service_was_running=false

    if [[ "$current_version" != "none" ]]; then
        log_info "Current installed version: $current_version"
        is_upgrade=true
        if is_service_running; then
            service_was_running=true
        fi
    else
        log_info "No existing installation found, performing fresh install"
    fi

    check_dependencies

    local os_arch
    os_arch=$(detect_linux_arch)
    log_step "Detected platform: $os_arch"

    # Find package in PACKAGE_DIR
    local pkg
    pkg=$(find_package "$os_arch")
    log_step "Using package: $pkg"

    # Extract version
    local version
    version=$(extract_version_from_package "$pkg")
    log_step "Package version: $version"

    # Check if already up to date
    if [[ "$is_upgrade" == true && "$current_version" == "$version" ]]; then
        log_success "Already up to date (version $version)"
        return
    fi

    # Stop service/processes if upgrading
    if [[ "$is_upgrade" == true ]]; then
        is_service_running && stop_service
        is_proxy_running && stop_proxy_processes
    fi

    # Backup config if upgrading
    local backup_file=""
    if [[ "$is_upgrade" == true ]]; then
        backup_file=$(backup_config)
    fi

    # Create version directory and extract
    local version_dir="${INSTALL_DIR}/${version}"
    mkdir -p "$INSTALL_DIR"
    extract_archive "$pkg" "$version_dir"

    # Setup config and copy executable
    setup_config "$version_dir" "$backup_file"

    # Create/update systemd service
    create_systemd_service "$INSTALL_DIR"

    # Write version file
    write_version_file "$INSTALL_DIR" "$version"

    # Cleanup old versions
    cleanup_old_versions "$version"

    # Restart service if it was running
    if [[ "$is_upgrade" == true && "$service_was_running" == true ]]; then
        restart_service
    fi

    # Success message
    if [[ "$is_upgrade" == true ]]; then
        log_success "Upgraded from $current_version to $version!"
        log_info "Installation directory: $INSTALL_DIR"
        if [[ "$service_was_running" == true ]]; then
            log_info "Service has been restarted automatically"
        else
            log_info "To start the service: systemctl --user start cliproxyapi.service"
        fi
    else
        log_success "Installed successfully! (version $version)"
        log_info "Installation directory: $INSTALL_DIR"
        show_authentication_info
        show_quick_start "$INSTALL_DIR"
    fi
}

# Show current status
show_status() {
    local current_version
    current_version=$(get_current_version)

    echo "Proxy Server Installation Status"
    echo "================================="
    echo "Install Directory: $INSTALL_DIR"
    echo "Package Directory: $PACKAGE_DIR"
    echo "Current Version:   $current_version"

    if [[ "$current_version" != "none" ]]; then
        if [[ -f "${INSTALL_DIR}/config.yaml" ]]; then
            echo "Configuration: Present"
        else
            echo "Configuration: Missing"
        fi
        if [[ -f "${INSTALL_DIR}/cli-proxy-api" ]]; then
            echo "Executable: Present"
        else
            echo "Executable: Missing"
        fi
        if is_service_running; then
            echo -e "Service: ${GREEN}Running${NC}"
        else
            echo -e "Service: ${YELLOW}Stopped${NC}"
        fi
        if check_api_keys; then
            echo -e "API Keys: ${GREEN}Configured${NC}"
        else
            echo -e "API Keys: ${YELLOW}NOT CONFIGURED${NC} - Edit config.yaml"
        fi
    else
        echo "Status: Not installed"
    fi
}

# Uninstall
uninstall_proxy() {
    if [[ ! -d "$INSTALL_DIR" ]]; then
        log_warning "Installation directory not found: $INSTALL_DIR"
        exit 0
    fi

    log_info "Installation found at: $INSTALL_DIR"
    read -p "Are you sure you want to remove the proxy server? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        log_info "Uninstallation cancelled"
        exit 0
    fi

    is_service_running && stop_service
    is_proxy_running && stop_proxy_processes

    rm -rf "$INSTALL_DIR"
    log_success "Proxy server has been uninstalled"
}

# Cleanup on exit
cleanup() {
    find /tmp -name "tmp.*" -user "$(whoami)" -delete 2>/dev/null || true
}
trap cleanup EXIT

# Main
main() {
    case "${1:-install}" in
        "install"|"upgrade")
            install_proxy
            ;;
        "status")
            show_status
            ;;
        "auth")
            show_authentication_info
            is_installed && show_quick_start "$INSTALL_DIR"
            ;;
        "check-config")
            if is_installed; then
                echo "Configuration Check"
                echo "==================="
                if check_api_keys; then
                    echo -e "✅ API Keys: ${GREEN}Configured${NC}"
                    echo -e "✅ Status:   ${GREEN}Ready to run${NC}"
                else
                    echo -e "❌ API Keys: ${RED}NOT CONFIGURED${NC}"
                    show_api_key_setup
                fi
            else
                log_error "Not installed. Run: $SCRIPT_NAME install"
            fi
            ;;
        "generate-key")
            local new_key
            new_key=$(generate_api_key)
            echo "Generated API Key:"
            echo "=================="
            echo -e "${GREEN}$new_key${NC}"
            echo
            echo -e "${YELLOW}Add it to ${INSTALL_DIR}/config.yaml under api-keys.${NC}"
            ;;
        "uninstall")
            uninstall_proxy
            ;;
        "-h"|"--help")
            cat << EOF
Proxy Server Linux Installer

Usage: $SCRIPT_NAME [COMMAND]

Commands:
  install, upgrade    Install or upgrade (reads package from $PACKAGE_DIR)
  status              Show current installation status
  auth                Show authentication setup information
  check-config        Check configuration and API keys
  generate-key        Generate a new API key
  uninstall           Remove the proxy server completely
  -h, --help          Show this help message

Package directory: $PACKAGE_DIR
Install directory: $INSTALL_DIR

Place a .tar.gz release package in $PACKAGE_DIR before running install.
EOF
            ;;
        *)
            log_error "Unknown command: $1"
            echo "Use '$SCRIPT_NAME --help' for usage information"
            exit 1
            ;;
    esac
}

main "$@"
