# gMSA-Helper

`gMSA-Helper` lets a domain-joined Linux host obtain and expose Kerberos credentials for Active Directory group Managed Service Accounts (gMSAs) by using AWS `credentials-fetcher`.

The helper does not retrieve or manage the gMSA password itself. It discovers the joined AD environment, builds the credentials specification required by `credentials-fetcher`, requests a Kerberos lease, and exposes the resulting cache through a stable per-gMSA path.

AWS credentials-fetcher: https://github.com/aws/credentials-fetcher

> This project was developed and tested on RHEL 10. AWS currently documents Amazon Linux 2023 and Fedora 41+ as supported platforms for credentials-fetcher. Check the upstream repository for the current support matrix.

## Architecture

```text
Active Directory
      |
      | machine account / Kerberos / LDAP
      v
credentials-fetcher.service
      |
      | AddKerberosLease / DeleteKerberosLease
      v
gmsa-helper@<NAME>.service
      |
      +--> GMSA_NAME=gMSA-<NAME>
      +--> /run/gmsa-helper-<NAME>/krb5cc
                         |
                         v
                    Application
```

One helper binary can manage multiple gMSAs on the same host. Each gMSA gets its own systemd instance, lease state, runtime directory, and stable Kerberos cache.

Example:

```text
gmsa-helper@Mailmgt.service   -> gMSA-Mailmgt   -> /run/gmsa-helper-Mailmgt/krb5cc
gmsa-helper@Reporting.service -> gMSA-Reporting -> /run/gmsa-helper-Reporting/krb5cc
```

The convention assumed by the systemd template is:

```text
gMSA-<NAME>
```

---

# Prerequisites

## 1. Linux host joined to Active Directory

The host must already be joined to AD and have a valid machine Kerberos keytab.

Validate:

```bash
realm list
adcli info <AD-DOMAIN>
sudo klist -k /etc/krb5.keytab
sudo systemctl status sssd --no-pager
```

AD DNS discovery must work:

```bash
dig _kerberos._tcp.<AD-DOMAIN> SRV
dig _ldap._tcp.dc._msdcs.<AD-DOMAIN> SRV
```

## 2. gMSA exists and the Linux computer is authorized

The gMSA must already exist in Active Directory.

The Linux computer account, or an AD group containing that computer account, must be authorized through:

```text
PrincipalsAllowedToRetrieveManagedPassword
```

Verify from a domain controller:

```powershell
Get-ADServiceAccount gMSA-<NAME> `
  -Properties PrincipalsAllowedToRetrieveManagedPassword |
Select-Object Name, PrincipalsAllowedToRetrieveManagedPassword
```

If the host is not authorized, credentials-fetcher can return an error similar to:

```text
failed to find gMSA password for service account <name>
```

## 3. AWS credentials-fetcher installed and running

Install credentials-fetcher by following the upstream AWS project:

https://github.com/aws/credentials-fetcher

For the source-based deployment used in this lab:

```bash
cd /opt
sudo git clone https://github.com/aws/credentials-fetcher.git
sudo chown -R "$(id -u)":"$(id -g)" /opt/credentials-fetcher
cd /opt/credentials-fetcher

make build
sudo make cf-install

sudo systemctl daemon-reload
sudo systemctl enable --now credentials-fetcher
```

Validate:

```bash
sudo systemctl status credentials-fetcher --no-pager
sudo journalctl -u credentials-fetcher -n 80 --no-pager

ls -l /usr/sbin/credentials-fetcher
ls -l /usr/lib/systemd/system/credentials-fetcher.service
sudo ls -l /var/credentials-fetcher/socket/
sudo ls -ld /var/credentials-fetcher/krbdir
```

Expected socket:

```text
/var/credentials-fetcher/socket/credentials_fetcher.sock
```

---

# Dependency baseline

The included `deployment-validation.sh` uses the following package baseline.

## Build dependencies

```text
make
git
curl
krb5-devel
Go toolchain (go-toolset when installation is required)
```

## Host/runtime packages

```text
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
```

The validator also verifies the required commands, including:

```text
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
```

## Lab application packages

```text
php-cli
php-ldap
```

Optional web stack:

```text
php-fpm
nginx
```

The PHP packages are application/test dependencies, not dependencies of the helper itself.

---

# Pre-deployment validation

Make the validator executable:

```bash
chmod +x deployment-validation.sh
```

Run it after the host is domain joined and credentials-fetcher is installed:

```bash
sudo ./deployment-validation.sh
```

The script asks for the AD domain, validates the required packages and services, and offers to install missing RPM packages with `dnf`.

Automatically accept missing-package installation:

```bash
sudo ./deployment-validation.sh --yes
```

Validate the optional PHP-FPM/Nginx stack:

```bash
sudo ./deployment-validation.sh --web-stack
```

---

# Build gMSA-Helper

## Clone the private repository

Because the repository is private, configure SSH authentication for GitHub on the RHEL host first.

If the host does not already have a GitHub SSH key:

```bash
ssh-keygen -t ed25519 -C "gmsa-helper-deploy"
cat ~/.ssh/id_ed25519.pub
```

Add the displayed public key to GitHub. It can be added to your GitHub account under **Settings > SSH and GPG keys**, or as a repository deploy key if you want the server key scoped only to this repository.

Validate GitHub authentication:

```bash
ssh -T git@github.com
```

A successful test returns a message similar to:

```text
Hi <GitHub-user>! You've successfully authenticated, but GitHub does not provide shell access.
```

GitHub may return exit status `1` for this test because it does not provide shell access; the authentication message is what matters.

Prepare the target directory under `/opt`:

```bash
sudo mkdir -p /opt/gMSA-Helper
sudo chown -R "$USER":"$(id -gn)" /opt/gMSA-Helper
```

Clone the repository **as the normal user, not with `sudo`**, so Git can use the SSH key in `~/.ssh`:

```bash
git clone git@github.com:MedElhamm0uti/gMSA-Helper.git /opt/gMSA-Helper
cd /opt/gMSA-Helper
```

Do not use:

```bash
sudo git clone git@github.com:MedElhamm0uti/gMSA-Helper.git
```

because `sudo` runs Git as `root`, which normally does not have access to the SSH private key stored in the deployment user's home directory.

Inspect which credentials-fetcher proto will be used for contract validation:

```bash
make print-config
```

With the source deployment above, the expected proto source is:

```text
/opt/credentials-fetcher/internal/grpc/proto/credentialsfetcher.proto
```

Build:

```bash
make build
```

If `go.sum` is not present yet, the Makefile runs `go mod tidy` on the first build. The build host therefore needs access to the configured Go module proxy/source unless the required modules are already cached internally. Commit the generated `go.sum` after the first successful build for reproducible subsequent builds.

The build performs these steps:

1. Copies the credentials-fetcher proto from the local source checkout into `.build/` for validation.
2. Validates the exact gRPC/protobuf methods and fields used by `gmsa-helper`.
3. Verifies the Go module dependencies.
4. Builds `.build/gmsa-helper`.

The proto is a build-time validation input only. It is not required by the helper at runtime.

### Alternative proto source

If credentials-fetcher was not built from `/opt/credentials-fetcher`, provide the exact proto explicitly:

```bash
make build PROTO_SOURCE=/path/to/credentialsfetcher.proto
```

If credentials-fetcher is installed as an RPM, the Makefile can also detect its version and fetch the corresponding tagged proto.

---

# Install gMSA-Helper

After a successful build, install the already-built binary and systemd template:

```bash
sudo make install
```

`make install` does **not** rebuild the project as root. It requires `.build/gmsa-helper` to already exist and installs:

```text
.build/gmsa-helper
    -> /usr/local/libexec/gmsa-helper

gmsa-helper@.service
    -> /etc/systemd/system/gmsa-helper@.service
```

`make install` does **not** run `systemctl daemon-reload`. Reload systemd explicitly after installation:

```bash
sudo systemctl daemon-reload
```

The normal deployment sequence is therefore:

```bash
make build
sudo make install
sudo systemctl daemon-reload
```

There is only one service template. Do not create a new unit file for every application.

---

# Start a gMSA instance

The systemd instance name is the part after `gMSA-`.

For the AD account:

```text
gMSA-Mailmgt
```

start:

```bash
sudo systemctl enable --now gmsa-helper@Mailmgt.service
```

The template automatically sets:

```text
GMSA_NAME=gMSA-Mailmgt
GMSA_STATE_DIR=/run/gmsa-helper-Mailmgt
```

The stable cache becomes:

```text
/run/gmsa-helper-Mailmgt/krb5cc
```

Validate the service:

```bash
sudo systemctl status gmsa-helper@Mailmgt.service --no-pager
sudo journalctl -u gmsa-helper@Mailmgt.service -n 80 --no-pager
```

Validate the Kerberos lease:

```bash
sudo ls -la /run/gmsa-helper-Mailmgt/
sudo readlink -f /run/gmsa-helper-Mailmgt/krb5cc
sudo klist -c FILE:/run/gmsa-helper-Mailmgt/krb5cc
```

A successful result should show a TGT for `gMSA-Mailmgt`.

---

# Validate LDAP/GSSAPI with the gMSA

Before integrating the application, prove that the Kerberos cache can authenticate to AD.

```bash
sudo env \
  KRB5CCNAME=FILE:/run/gmsa-helper-Mailmgt/krb5cc \
  ldapwhoami \
  -Y GSSAPI \
  -H ldap://<DOMAIN-CONTROLLER>
```

Example LDAP query:

```bash
sudo env \
  KRB5CCNAME=FILE:/run/gmsa-helper-Mailmgt/krb5cc \
  ldapsearch \
  -Y GSSAPI \
  -H ldap://<DOMAIN-CONTROLLER> \
  -b "DC=example,DC=com" \
  -s base
```

This validates the full path:

```text
AD machine account
      -> credentials-fetcher
      -> gMSA managed password retrieval
      -> Kerberos lease
      -> stable cache
      -> LDAP/GSSAPI
```

---

# Post-deployment validation

Run:

```bash
sudo ./deployment-validation.sh --post-deploy
```

The script asks for:

```text
Active Directory domain
Instance name without the gMSA- prefix
```

For `gMSA-Mailmgt`, enter:

```text
Mailmgt
```

It validates:

```text
/usr/local/libexec/gmsa-helper
/etc/systemd/system/gmsa-helper@.service
gmsa-helper@Mailmgt.service
/run/gmsa-helper-Mailmgt/krb5cc
Kerberos ticket validity
```

---

# Multiple gMSAs on the same host

No additional helper binaries or service files are required.

For:

```text
gMSA-Mailmgt
gMSA-Reporting
gMSA-App2
```

start three isolated instances:

```bash
sudo systemctl enable --now gmsa-helper@Mailmgt.service
sudo systemctl enable --now gmsa-helper@Reporting.service
sudo systemctl enable --now gmsa-helper@App2.service
```

Result:

```text
gmsa-helper@Mailmgt.service
  -> gMSA-Mailmgt
  -> /run/gmsa-helper-Mailmgt/krb5cc

gmsa-helper@Reporting.service
  -> gMSA-Reporting
  -> /run/gmsa-helper-Reporting/krb5cc

gmsa-helper@App2.service
  -> gMSA-App2
  -> /run/gmsa-helper-App2/krb5cc
```

Each instance has its own lease ID, state files, symlink, and credentials-fetcher Kerberos cache.

---

# Application integration

An application consumes the stable cache through `KRB5CCNAME`.

For MailMgt:

```text
KRB5CCNAME=FILE:/run/gmsa-helper-Mailmgt/krb5cc
```

For a dedicated systemd application service, add a drop-in such as:

```ini
[Unit]
Requires=gmsa-helper@Mailmgt.service
After=gmsa-helper@Mailmgt.service

[Service]
Environment="KRB5CCNAME=FILE:/run/gmsa-helper-Mailmgt/krb5cc"
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl restart <APPLICATION-SERVICE>
```

## PHP-FPM lab example

For the single-application PHP lab, the same dependency/environment can be placed in a PHP-FPM systemd drop-in.

Important: if one shared PHP-FPM service hosts multiple applications that require different gMSAs, do not assign one global `KRB5CCNAME` to that shared service. The applications need separate process/service isolation so each receives the correct cache.

---

# Application cache permissions

The helper runs as root and each runtime directory is created with mode `0700`:

```text
/run/gmsa-helper-<NAME>/
```

Therefore a non-root application such as PHP-FPM will not automatically be able to read the cache even though the helper and LDAP tests succeed as root.

Before the application test, identify the application service account and inspect the entire path:

```bash
namei -l /run/gmsa-helper-Mailmgt/krb5cc
```

Grant only the minimum required access to the application account. The application must be able to traverse the helper runtime directory and read the underlying credentials-fetcher cache target.

Do not make the Kerberos cache globally readable.

---

# Service lifecycle

Starting an instance:

```bash
sudo systemctl start gmsa-helper@Mailmgt.service
```

causes the helper to:

```text
discover AD
-> authenticate with the machine keytab
-> discover the configured gMSA
-> build the CredSpec in memory
-> call AddKerberosLease
-> validate the returned Kerberos cache
-> create the stable krb5cc symlink
```

Stopping it:

```bash
sudo systemctl stop gmsa-helper@Mailmgt.service
```

causes the helper to call `DeleteKerberosLease` and remove its local runtime state.

Kerberos ticket refresh during the active lease is handled by `credentials-fetcher`.

---

# Troubleshooting

## `failed to find gMSA password`

Verify that the current Linux computer account is authorized in the gMSA's `PrincipalsAllowedToRetrieveManagedPassword`.

## credentials-fetcher socket missing

```bash
sudo systemctl status credentials-fetcher --no-pager
sudo journalctl -u credentials-fetcher -n 80 --no-pager
sudo ls -l /var/credentials-fetcher/socket/
```

Expected:

```text
/var/credentials-fetcher/socket/credentials_fetcher.sock
```

## Helper refuses to start because lease state already exists

Check the matching instance and state directory:

```bash
sudo systemctl status gmsa-helper@<NAME>.service
sudo ls -la /run/gmsa-helper-<NAME>/
```

Do not manually remove a recorded lease ID unless the credentials-fetcher lease lifecycle has been understood. Prefer stopping/restarting the systemd instance normally.

## Application cannot use the cache

First prove the cache itself is valid:

```bash
sudo klist -c FILE:/run/gmsa-helper-<NAME>/krb5cc
```

Then inspect application permissions:

```bash
namei -l /run/gmsa-helper-<NAME>/krb5cc
```

---

# Security notes

- The gMSA managed password is not stored in the helper configuration.
- The CredSpec is generated dynamically in memory.
- The application consumes a Kerberos cache rather than the gMSA password.
- Each gMSA instance has an isolated state directory and lease lifecycle.
- Keep the credentials-fetcher socket and Kerberos cache paths restricted.
- Grant application cache access only to the service identity that needs it.
- Build-time protobuf validation protects the helper from silently compiling against an incompatible credentials-fetcher contract.

---

# Upstream dependency

AWS credentials-fetcher:

https://github.com/aws/credentials-fetcher
