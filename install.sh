#!/usr/bin/env bash
set -eo pipefail

# ============================================================
# Claude Code Gateway 交互式安装脚本
# ============================================================

REPO="xyzensun/ccgateway"
BINARY_NAME="claude-code-gateway"
SCRIPT_NAME="ccgateway.sh"

# ── 颜色输出 ──
info()    { printf '\033[1;34m%s\033[0m\n' "$*"; }
success() { printf '\033[1;38;5;208m%s\033[0m\n' "$*"; }
warn()    { printf '\033[1;33m%s\033[0m\n' "$*"; }
error()   { printf '\033[1;31mError: %s\033[0m\n' "$*" >&2; exit 1; }

# ── 交互式读取 ──
prompt_choice() {
    local prompt_text="$1"
    local default="$2"
    shift 2
    local options=("$@")
    local opts_display=""
    local i=1
    for opt in "${options[@]}"; do
        if [[ "$opt" == "$default" ]]; then
            opts_display+="  \033[1;36m[$i]\033[0m $opt \033[1;33m(default)\033[0m\n"
        else
            opts_display+="  \033[1;36m[$i]\033[0m $opt\n"
        fi
        i=$((i + 1))
    done

    printf "\n\033[1;35m%s\033[0m\n" "$prompt_text"
    printf "%b" "$opts_display"
    printf "\033[1;36mYour choice [1-%d]: \033[0m" "${#options[@]}"

    local choice
    read -r choice
    if [[ -z "$choice" ]]; then
        choice=""
        for opt in "${options[@]}"; do
            if [[ "$opt" == "$default" ]]; then
                PROMPT_RESULT="$opt"
                break
            fi
        done
    else
        if [[ "$choice" =~ ^[0-9]+$ ]] && [[ "$choice" -ge 1 ]] && [[ "$choice" -le "${#options[@]}" ]]; then
            PROMPT_RESULT="${options[$((choice - 1))]}"
        else
            PROMPT_RESULT="$choice"
        fi
    fi
}

prompt_input() {
    local prompt_text="$1"
    local default="$2"
    printf "\n\033[1;35m%s\033[0m \033[1;33m(default: %s)\033[0m: " "$prompt_text" "$default"
    local input
    read -r input
    if [[ -z "$input" ]]; then
        PROMPT_RESULT="$default"
    else
        PROMPT_RESULT="$input"
    fi
}

# ── 平台检测 ──
detect_platform() {
    local os arch target
    os="$(uname -s)"
    arch="$(uname -m)"

    case "$os" in
        Linux)
            case "$arch" in
                x86_64)        target="x86_64-linux" ;;
                aarch64|arm64) target="aarch64-linux" ;;
                *) error "Unsupported architecture: $arch" ;;
            esac
            ;;
        Darwin)
            case "$arch" in
                x86_64)        target="x86_64-darwin" ;;
                aarch64|arm64) target="aarch64-darwin" ;;
                *) error "Unsupported architecture: $arch" ;;
            esac
            ;;
        MINGW*|MSYS*|CYGWIN*)
            case "$arch" in
                x86_64)        target="x86_64-windows" ;;
                aarch64|arm64) target="aarch64-windows" ;;
                *) error "Unsupported architecture: $arch" ;;
            esac
            ;;
        *) error "Unsupported OS: $os" ;;
    esac
    echo "$target"
}

# ── 获取最新 release tag ──
get_latest_release_tag() {
    local api_url="${DOWNLOAD_PROXY}https://api.github.com/repos/${REPO}/releases/latest"
    local releases_json
    releases_json=$(curl -fsSL "$api_url") \
        || error "Failed to fetch releases from GitHub"

    local tag
    tag=$(echo "$releases_json" | grep -oE '"tag_name": *"[^"]*"' | head -1 | sed 's/"tag_name": *"/"/;s/"$//')

    if [ -z "$tag" ]; then
        error "No release found."
    fi
    echo "$tag"
}

# ── 下载文件带进度 ──
download_with_progress() {
    local output="$1"
    local url="$2"
    local desc="$3"

    if curl --help 2>&1 | grep -q -- '--progress-bar'; then
        printf '\033[1;34m  %s\033[0m\n' "$desc"
        if ! curl -fSL --progress-bar -o "$output" "$url"; then
            printf '\n\033[1;31mError: Failed to download.\033[0m\n' >&2
            echo "  URL: ${url}" >&2
            return 1
        fi
        printf '\n'
    elif command -v wget &>/dev/null; then
        printf '\033[1;34m  %s\033[0m\n' "$desc"
        if ! wget -q --show-progress -O "$output" "$url"; then
            printf '\033[1;31mError: Failed to download.\033[0m\n' >&2
            echo "  URL: ${url}" >&2
            return 1
        fi
    else
        printf '\033[1;34m  %s\033[0m\n' "$desc"
        if ! curl -fsSL -o "$output" "$url"; then
            printf '\033[1;31mError: Failed to download.\033[0m\n' >&2
            echo "  URL: ${url}" >&2
            return 1
        fi
    fi
    return 0
}

# ── 下载二进制和管理脚本 ──
download_release() {
    local target="$1"
    local tag="$2"
    local install_dir="$3"
    local ext=""

    case "$target" in
        *windows*) ext=".exe" ;;
    esac

    local bin_filename="${BINARY_NAME}-${target}${ext}"
    local script_filename="${SCRIPT_NAME}"
    local github_base="https://github.com/${REPO}/releases/download/${tag}"

    local bin_url="${DOWNLOAD_PROXY}${github_base}/${bin_filename}"
    local checksum_url="${DOWNLOAD_PROXY}${github_base}/${bin_filename}.sha256"
    local script_url="${DOWNLOAD_PROXY}${github_base}/${script_filename}"

    if [[ -n "$DOWNLOAD_PROXY" ]]; then
        info "Using proxy: ${DOWNLOAD_PROXY}"
    fi

    local tmp_dir
    tmp_dir="$(mktemp -d)"
    trap 'rm -rf "$tmp_dir"' EXIT

    # 下载二进制
    info "Downloading ${bin_filename} from release ${tag}..."
    if ! download_with_progress "${tmp_dir}/${bin_filename}" "$bin_url" "Downloading binary: ${bin_filename}"; then
        echo "" >&2
        echo "  Release: ${tag}" >&2
        echo "  Platform: ${target}" >&2
        echo "If using a proxy, try a different one or switch to 'No proxy'." >&2
        exit 1
    fi

    # 校验 checksum
    if command -v sha256sum &>/dev/null; then
        if download_with_progress "${tmp_dir}/${bin_filename}.sha256" "$checksum_url" "Downloading checksum"; then
            info "Verifying checksum..."
            (cd "$tmp_dir" && sha256sum -c "${bin_filename}.sha256") \
                || error "Checksum verification failed!"
        else
            warn "Checksum file not available, skipping verification."
        fi
    fi

    # 下载管理脚本
    info "Downloading management script..."
    if download_with_progress "${tmp_dir}/${script_filename}" "$script_url" "Downloading: ${script_filename}"; then
        success "Management script downloaded."
    else
        warn "Management script not found in release, skipping."
    fi

    # 安装到目标目录
    mkdir -p "$install_dir/data"
    mv "${tmp_dir}/${bin_filename}" "${install_dir}/${BINARY_NAME}${ext}"
    chmod +x "${install_dir}/${BINARY_NAME}${ext}"

    if [ -f "${tmp_dir}/${script_filename}" ]; then
        mv "${tmp_dir}/${script_filename}" "${install_dir}/${script_filename}"
        chmod +x "${install_dir}/${script_filename}"
    fi

    success "Installed ${BINARY_NAME} to ${install_dir}/${BINARY_NAME}${ext}"
}

# ── PATH 检查 ──
check_path() {
    local install_dir="$1"
    case ":$PATH:" in
        *":${install_dir}:"*) return 0 ;;
    esac

    warn "${install_dir} is not in your PATH."
    echo ""
    echo "Add it to your shell profile:"
    echo ""

    local shell_name
    shell_name="$(basename "${SHELL:-bash}")"
    case "$shell_name" in
        zsh)
            echo "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.zshrc"
            echo "  source ~/.zshrc"
            ;;
        fish)
            echo "  fish_add_path ${install_dir}"
            ;;
        *)
            echo "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.bashrc"
            echo "  source ~/.bashrc"
            ;;
    esac
    echo ""
}

# ── 输出配置指引 ──
print_setup_instructions() {
    local install_dir="$1"

    echo ""
    success "Claude Code Gateway installed successfully!"
    echo ""
    info "Quick start:"
    echo ""
    echo "  cd ${install_dir}"
    echo "  ./ccgateway.sh --start --port 9999 --password yourpassword"
    echo ""
    info "Or use the 'ccgateway' shortcut (source the wrapper):"
    echo ""
    echo "  source ${install_dir}/ccgateway.sh"
    echo "  ccgateway start --port 9999"
    echo ""
    echo "  Admin panel: http://localhost:9999/admin"
    echo ""
    echo "Binary:  ${install_dir}/${BINARY_NAME}"
    echo "Script:  ${install_dir}/${SCRIPT_NAME}"
    echo "Docs:    https://github.com/${REPO}"
    echo ""
}

# ============================================================
# 主流程
# ============================================================
main() {
    echo ""
    info "=== Claude Code Gateway 交互式安装 ==="
    echo ""

    # ── 1. 选择安装目录 ──
    local default_dir="$HOME/.ccgateway"
    prompt_input "Install directory" "$default_dir"
    INSTALL_DIR="$PROMPT_RESULT"
    if [[ "$INSTALL_DIR" != /* ]]; then
        INSTALL_DIR="$(pwd)/${INSTALL_DIR}"
    fi

    # ── 2. 选择下载代理 ──
    echo ""
    info "GitHub download proxy options:"
    prompt_choice "Use a GitHub download proxy?" "No proxy" "No proxy" "Default: https://cdn.gh-proxy.org/" "Custom"

    case "$PROMPT_RESULT" in
        "No proxy")
            DOWNLOAD_PROXY=""
            ;;
        "Default: https://cdn.gh-proxy.org/")
            DOWNLOAD_PROXY="https://cdn.gh-proxy.org/"
            ;;
        "Custom")
            prompt_input "Enter your custom proxy URL (must end with /)" "https://cdn.gh-proxy.org/"
            DOWNLOAD_PROXY="$PROMPT_RESULT"
            if [[ "$DOWNLOAD_PROXY" != */ ]]; then
                DOWNLOAD_PROXY="${DOWNLOAD_PROXY}/"
            fi
            ;;
        *)
            DOWNLOAD_PROXY="$PROMPT_RESULT"
            if [[ "$DOWNLOAD_PROXY" != */ ]]; then
                DOWNLOAD_PROXY="${DOWNLOAD_PROXY}/"
            fi
            ;;
    esac

    echo ""
    info "Configuration:"
    echo "  Install dir:    ${INSTALL_DIR}"
    echo "  Download proxy: ${DOWNLOAD_PROXY:-<none, direct from GitHub>}"
    echo ""

    # ── 3. 平台检测 ──
    local target
    target="$(detect_platform)"
    info "Detected platform: ${target}"

    # ── 4. 获取版本 ──
    local tag
    tag="$(get_latest_release_tag)"
    info "Latest release: ${tag}"

    # ── 5. 检查是否为更新 ──
    local existing_binary="${INSTALL_DIR}/${BINARY_NAME}"
    IS_UPDATE=false
    if [ -x "$existing_binary" ]; then
        IS_UPDATE=true
        info "Updating existing installation..."
    else
        info "Installing Claude Code Gateway..."
    fi

    # ── 6. 下载 ──
    download_release "$target" "$tag" "$INSTALL_DIR"

    # ── 7. 输出配置 ──
    if [ "$IS_UPDATE" = true ]; then
        echo ""
        success "Claude Code Gateway updated to ${tag}!"
        echo ""
    else
        check_path "$INSTALL_DIR"
        print_setup_instructions "$INSTALL_DIR"
    fi
}

main