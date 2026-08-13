# Build and deployment notes

## Build

The canonical build entry point is:

```bash
make build
```

The Makefile first validates the embedded gRPC/protobuf subset against credentials-fetcher.

Source selection order:

1. `PROTO_SOURCE` when explicitly supplied.
2. `/opt/credentials-fetcher/internal/grpc/proto/credentialsfetcher.proto` when the AWS source checkout exists.
3. The proto from the detected/pinned credentials-fetcher RPM version.

Inspect the selected source with:

```bash
make print-config
```

The proto is copied into `.build/upstream/credentialsfetcher.proto` for validation and is not a runtime dependency.

Build output:

```text
.build/gmsa-helper
```

## Install

```bash
sudo install -d -o root -g root -m 0755 /usr/local/libexec

sudo install -o root -g root -m 0750 \
  .build/gmsa-helper \
  /usr/local/libexec/gmsa-helper

sudo install -o root -g root -m 0644 \
  gmsa-helper@.service \
  /etc/systemd/system/gmsa-helper@.service

sudo systemctl daemon-reload
```

## Instance model

The systemd instance name is the part after `gMSA-`.

```text
gMSA-Mailmgt -> gmsa-helper@Mailmgt.service
gMSA-App2    -> gmsa-helper@App2.service
```

The template derives:

```text
GMSA_NAME=gMSA-<NAME>
GMSA_STATE_DIR=/run/gmsa-helper-<NAME>
```

Example:

```bash
sudo systemctl enable --now gmsa-helper@Mailmgt.service
sudo klist -c FILE:/run/gmsa-helper-Mailmgt/krb5cc
```

## Runtime commands used by the helper

```text
realm
adcli
kinit
klist
kdestroy
ldapsearch
```

The helper does not require `grpc_cli`, `protoc`, EPEL, or a runtime `.proto` file.


## Bind an application service

Bind a systemd application service to a helper instance:

```bash
sudo /usr/local/libexec/gmsa-helper bind \
  --service php-fpm.service \
  --instance Mailmgt
```

This creates:

```text
/etc/systemd/system/php-fpm.service.d/gmsa-helper.conf
```

with the dependency on `gmsa-helper@Mailmgt.service` and:

```text
KRB5CCNAME=FILE:/run/gmsa-helper-Mailmgt/krb5cc
```

The command reloads systemd and verifies the effective dependency and environment. It does not restart the application.

The operation is idempotent. Use `--force` only when intentionally replacing an existing `gmsa-helper.conf`.
