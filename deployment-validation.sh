#!/usr/bin/env bash

set -u

DOMAIN="${DOMAIN:-}"

CHECK_WEB_STACK=0
CHECK_DEPLOYMENT=0
AUTO_YES=0
INSTANCE="${INSTANCE:-}"

PASS=0
FAIL=0
WARN=0

MISSING_PACKAGES=()


# ============================================================
# Colors
# ============================================================

if [[ -t 1 ]]; then
    RESET="\033[0m"
    RED="\033[31m"
    GREEN="\033[32m"
    YELLOW="\033[33m"
    BLUE="\033[34m"
    CYAN="\033[36m"
    BOLD="\033[1m"
else
    RESET=""
    RED=""
    GREEN=""
    YELLOW=""
    BLUE=""
    CYAN=""
    BOLD=""
fi


# ============================================================
# Output helpers
# ============================================================

section() {
    echo
    printf "${BOLD}${CYAN}========================================${RESET}\n"
    printf "${BOLD}${CYAN} %s${RESET}\n" "$1"
    printf "${BOLD}${CYAN}========================================${RESET}\n"
}

pass() {
    printf "${GREEN}[PASS]${RESET} %s\n" "$1"
    ((PASS++))
}

fail() {
    printf "${RED}[FAIL]${RESET} %s\n" "$1"
    ((FAIL++))
}

warn() {
    printf "${YELLOW}[WARN]${RESET} %s\n" "$1"
    ((WARN++))
}

info() {
    printf "${BLUE}[INFO]${RESET} %s\n" "$1"
}


# ============================================================
# Arguments
# ============================================================

for arg in "$@"; do
    case "$arg" in
        --web-stack)
            CHECK_WEB_STACK=1
            ;;

        --post-deploy)
            CHECK_DEPLOYMENT=1
            ;;

        --yes|-y)
            AUTO_YES=1
            ;;

        *)
            echo "Unknown option: $arg"
            echo
            echo "Usage:"
            echo "  $0 [--web-stack] [--post-deploy] [--yes]"
            exit 2
            ;;
    esac
done


# ============================================================
# Active Directory domain
# ============================================================

if [[ -z "$DOMAIN" ]]; then
    printf "${BOLD}${BLUE}Enter the Active Directory domain (e.g. MyLab.lu): ${RESET}"
    read -r DOMAIN
fi

if [[ -z "$DOMAIN" ]]; then
    printf "${BOLD}${RED}AD domain cannot be empty.${RESET}\n"
    exit 2
fi

if (( CHECK_DEPLOYMENT )) && [[ -z "$INSTANCE" ]]; then
    printf "${BOLD}${BLUE}Enter the gMSA instance name without the gMSA- prefix (e.g. Mailmgt): ${RESET}"
    read -r INSTANCE
fi

if (( CHECK_DEPLOYMENT )); then
    if [[ -z "$INSTANCE" ]]; then
        printf "${BOLD}${RED}gMSA instance name cannot be empty.${RESET}\n"
        exit 2
    fi

    if [[ ! "$INSTANCE" =~ ^[A-Za-z0-9._-]+$ ]]; then
        printf "${BOLD}${RED}Invalid gMSA instance name: %s${RESET}\n" "$INSTANCE"
        exit 2
    fi
fi


# ============================================================
# Package functions
# ============================================================

package_installed() {
    rpm -q "$1" &>/dev/null
}

add_missing_package() {
    local pkg="$1"
    local existing

    for existing in "${MISSING_PACKAGES[@]:-}"; do
        [[ "$existing" == "$pkg" ]] && return
    done

    MISSING_PACKAGES+=("$pkg")
}

check_package() {
    local pkg="$1"

    if package_installed "$pkg"; then
        pass "$(rpm -q "$pkg")"
    else
        fail "Package missing: $pkg"
        add_missing_package "$pkg"
    fi
}

check_command() {
    local cmd="$1"

    if command -v "$cmd" &>/dev/null; then
        pass "$cmd -> $(command -v "$cmd")"
    else
        fail "Command missing: $cmd"
    fi
}

check_file() {
    local file="$1"

    if [[ -f "$file" ]]; then
        pass "File exists: $file"
    else
        fail "File missing: $file"
    fi
}

check_service_loaded() {
    local service="$1"

    if systemctl cat "$service" &>/dev/null; then
        pass "systemd unit loaded: $service"
    else
        fail "systemd unit missing: $service"
    fi
}

check_service_active() {
    local service="$1"

    if systemctl is-active --quiet "$service"; then
        pass "Service active: $service"
    else
        fail "Service not active: $service"
    fi
}


# ============================================================
# Install missing packages
# ============================================================

install_missing_packages() {

    if (( ${#MISSING_PACKAGES[@]} == 0 )); then
        return
    fi

    section "MISSING PACKAGES"

    for pkg in "${MISSING_PACKAGES[@]}"; do
        printf "${RED} - %s${RESET}\n" "$pkg"
    done

    echo

    if (( AUTO_YES )); then
        answer="y"
    else
        printf "${BOLD}${BLUE}Install missing packages with dnf? [y/N]: ${RESET}"
        read -r answer
    fi

    case "$answer" in
        y|Y|yes|YES)

            info "Installing missing packages..."

            if [[ $EUID -eq 0 ]]; then
                dnf install -y "${MISSING_PACKAGES[@]}"
            else
                sudo dnf install -y "${MISSING_PACKAGES[@]}"
            fi

            if [[ $? -ne 0 ]]; then
                printf "${BOLD}${RED}Package installation failed.${RESET}\n"
                exit 1
            fi

            printf "${GREEN}Package installation completed.${RESET}\n"

            MISSING_PACKAGES=()
            ;;

        *)
            warn "Package installation skipped."
            ;;
    esac
}


# ============================================================
# BUILD dependencies
# ============================================================

section "BUILD DEPENDENCIES"

BUILD_PACKAGES=(
    make
    git
    curl
    krb5-devel
)

for pkg in "${BUILD_PACKAGES[@]}"; do
    check_package "$pkg"
done


# Go
if command -v go &>/dev/null; then
    pass "Go compiler -> $(go version)"
else
    fail "Go compiler missing"
    add_missing_package "go-toolset"
fi


# ============================================================
# HOST / runtime prerequisites
# ============================================================

section "HOST / RUNTIME PACKAGES"

RUNTIME_PACKAGES=(
    realmd
    adcli
    sssd
    sssd-tools
    oddjob
    oddjob-mkhomedir
    krb5-workstation
    openldap-clients
    samba-common-tools
    acl
    dnsmasq
    bind-utils
    pam
    systemd
)

for pkg in "${RUNTIME_PACKAGES[@]}"; do
    check_package "$pkg"
done


# ============================================================
# Application packages
# ============================================================

section "APPLICATION PACKAGES"

APP_PACKAGES=(
    php-cli
    php-ldap
)

for pkg in "${APP_PACKAGES[@]}"; do
    check_package "$pkg"
done


if (( CHECK_WEB_STACK )); then
    check_package "php-fpm"
    check_package "nginx"
fi


# ============================================================
# Offer package installation
# ============================================================

install_missing_packages


# ============================================================
# Reset counters and perform final validation
# ============================================================

PASS=0
FAIL=0
WARN=0


# ============================================================
# Package validation
# ============================================================

section "PACKAGE VALIDATION"

for pkg in "${BUILD_PACKAGES[@]}"; do
    check_package "$pkg"
done

for pkg in "${RUNTIME_PACKAGES[@]}"; do
    check_package "$pkg"
done

for pkg in "${APP_PACKAGES[@]}"; do
    check_package "$pkg"
done

if (( CHECK_WEB_STACK )); then
    check_package "php-fpm"
    check_package "nginx"
fi

if command -v go &>/dev/null; then
    pass "Go compiler -> $(go version)"
else
    fail "Go compiler missing"
fi


# ============================================================
# Required tools
# ============================================================

section "REQUIRED COMMANDS"

TOOLS=(
    realm
    adcli
    sssctl
    kinit
    klist
    kdestroy
    ldapsearch
    ldapwhoami
    setfacl
    getfacl
    dig
    nslookup
    faillock
    authselect
    systemctl
    make
    git
    curl
    go
)

for tool in "${TOOLS[@]}"; do
    check_command "$tool"
done


# ============================================================
# Active Directory
# ============================================================

section "ACTIVE DIRECTORY"

if realm list --name-only 2>/dev/null | grep -Fxiq "$DOMAIN"; then
    pass "Joined to AD domain: $DOMAIN"
else
    fail "Host is not joined to $DOMAIN"
fi


# Machine keytab
if [[ -s /etc/krb5.keytab ]]; then

    pass "/etc/krb5.keytab exists"

    if klist -k /etc/krb5.keytab &>/dev/null; then
        pass "Kerberos machine keytab readable"
    else
        fail "Kerberos machine keytab cannot be read"
    fi

else
    fail "/etc/krb5.keytab missing or empty"
fi


# AD discovery
if adcli info "$DOMAIN" &>/dev/null; then
    pass "adcli can discover $DOMAIN"
else
    fail "adcli cannot discover $DOMAIN"
fi


# ============================================================
# SSSD
# ============================================================

section "SSSD"

check_service_loaded "sssd.service"
check_service_active "sssd.service"

if sssctl config-check &>/dev/null; then
    pass "SSSD configuration valid"
else
    fail "SSSD configuration check failed"
fi


# ============================================================
# DNS / AD discovery
# ============================================================

section "DNS / AD DISCOVERY"

if dig +short "_kerberos._tcp.${DOMAIN}" SRV | grep -q .; then
    pass "Kerberos SRV records found"
else
    fail "No Kerberos SRV records found for $DOMAIN"
fi

if dig +short "_ldap._tcp.dc._msdcs.${DOMAIN}" SRV | grep -q .; then
    pass "LDAP/DC SRV records found"
else
    fail "No LDAP/DC SRV records found for $DOMAIN"
fi


# ============================================================
# credentials-fetcher
# ============================================================

section "CREDENTIALS-FETCHER"

# credentials-fetcher can be installed from source and therefore
# is not required to be registered as an RPM package.

if command -v credentials-fetcher &>/dev/null; then
    pass "credentials-fetcher -> $(command -v credentials-fetcher)"

elif [[ -x /usr/sbin/credentials-fetcher ]]; then
    pass "credentials-fetcher -> /usr/sbin/credentials-fetcher"

else
    fail "credentials-fetcher binary missing"
fi


check_service_loaded "credentials-fetcher.service"


if systemctl is-active --quiet credentials-fetcher.service; then

    pass "credentials-fetcher.service active"

    SOCKET="/var/credentials-fetcher/socket/credentials_fetcher.sock"

    if [[ -S "$SOCKET" ]]; then
        pass "Socket exists: $SOCKET"
    else
        fail "Socket missing: $SOCKET"
    fi


    if [[ -d /var/credentials-fetcher/krbdir ]]; then
        pass "/var/credentials-fetcher/krbdir exists"
    else
        fail "/var/credentials-fetcher/krbdir missing"
    fi

else
    fail "credentials-fetcher.service not active"
fi


# ============================================================
# PHP
# ============================================================

section "PHP"

check_command "php"

if php -m 2>/dev/null | grep -Fxiq "ldap"; then
    pass "PHP LDAP extension loaded"
else
    fail "PHP LDAP extension not loaded"
fi


# ============================================================
# Web stack
# ============================================================

if (( CHECK_WEB_STACK )); then

    section "PHP-FPM / NGINX"

    check_service_loaded "php-fpm.service"
    check_service_loaded "nginx.service"

    check_service_active "php-fpm.service"
    check_service_active "nginx.service"

fi


# ============================================================
# Our deployment
# ============================================================

if (( CHECK_DEPLOYMENT )); then

    section "GMSA HELPER DEPLOYMENT"

    SERVICE="gmsa-helper@${INSTANCE}.service"
    STATE_DIR="/run/gmsa-helper-${INSTANCE}"
    CACHE="${STATE_DIR}/krb5cc"
    EXPECTED_GMSA="gMSA-${INSTANCE}"

    check_file "/usr/local/libexec/gmsa-helper"

    check_file \
        "/etc/systemd/system/gmsa-helper@.service"

    check_service_loaded "$SERVICE"

    if systemctl is-active --quiet "$SERVICE"; then
        pass "$SERVICE active"
    else
        fail "$SERVICE not active"
    fi

    SERVICE_ENV="$(systemctl show "$SERVICE" -p Environment --value 2>/dev/null || true)"

    if grep -Fq "GMSA_NAME=${EXPECTED_GMSA}" <<<"$SERVICE_ENV"; then
        pass "$SERVICE configured for ${EXPECTED_GMSA}"
    else
        fail "$SERVICE does not expose GMSA_NAME=${EXPECTED_GMSA}"
    fi

    if grep -Fq "GMSA_STATE_DIR=${STATE_DIR}" <<<"$SERVICE_ENV"; then
        pass "$SERVICE uses state directory ${STATE_DIR}"
    else
        fail "$SERVICE does not expose GMSA_STATE_DIR=${STATE_DIR}"
    fi

    if [[ -L "$CACHE" ]]; then

        pass "Stable Kerberos cache symlink exists: $CACHE"

        TARGET="$(readlink -f "$CACHE")"

        if [[ -f "$TARGET" ]]; then

            pass "Kerberos cache target exists: $TARGET"

            if klist -c "FILE:${CACHE}" &>/dev/null; then
                pass "gMSA Kerberos ticket valid"
            else
                fail "gMSA Kerberos cache invalid"
            fi

        else
            fail "Kerberos cache symlink target missing"
        fi

    else
        fail "$CACHE missing"
    fi

fi


# ============================================================
# Result
# ============================================================

section "RESULT"

printf "${GREEN}PASS : %d${RESET}\n" "$PASS"
printf "${YELLOW}WARN : %d${RESET}\n" "$WARN"
printf "${RED}FAIL : %d${RESET}\n" "$FAIL"

echo

if (( FAIL > 0 )); then
    printf "${BOLD}${RED}PRECHECK FAILED${RESET}\n"
    exit 1
fi

printf "${BOLD}${GREEN}PRECHECK PASSED${RESET}\n"
exit 0
