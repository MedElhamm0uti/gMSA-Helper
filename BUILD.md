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

After the build succeeds:

```bash
sudo make install
```

The install target copies the already-built artifacts only. It does not build as root and does not reload systemd.

Installed paths:

```text
/usr/local/libexec/gmsa-helper
/etc/systemd/system/gmsa-helper@.service
```

Canonical sequence:

```bash
make build
sudo make install
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
