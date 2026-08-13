SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

APP := gmsa-helper
BUILD_DIR := .build
BINARY := $(BUILD_DIR)/$(APP)
PROTO_DIR := $(BUILD_DIR)/upstream
PROTO := $(PROTO_DIR)/credentialsfetcher.proto
LOCAL_CF_PROTO ?= /opt/credentials-fetcher/internal/grpc/proto/credentialsfetcher.proto

# Preferred validation source: the credentials-fetcher source checkout used
# to build the daemon on this host. If unavailable, fall back to an installed
# RPM version or an explicitly supplied PROTO_SOURCE/version.
CREDENTIALS_FETCHER_VERSION ?= $(shell rpm -q --qf '%{VERSION}' credentials-fetcher 2>/dev/null || true)
CREDENTIALS_FETCHER_TAG ?= v$(CREDENTIALS_FETCHER_VERSION)

# Optional build inputs for controlled/offline environments.
# PROTO_SOURCE=/path/to/approved/credentialsfetcher.proto avoids network access.
# PROTO_SHA256=<sha256> optionally verifies the downloaded/copied source.
PROTO_SOURCE ?=
PROTO_SHA256 ?=
PROTO_URL ?= https://raw.githubusercontent.com/aws/credentials-fetcher/$(CREDENTIALS_FETCHER_TAG)/internal/grpc/proto/credentialsfetcher.proto

INSTALL_BIN := /usr/local/libexec/gmsa-helper
INSTALL_UNIT := /etc/systemd/system/gmsa-helper@.service

.PHONY: all build proto validate-proto prepare-modules verify-modules install uninstall clean print-config

all: build

print-config:
	@echo "binary                      : $(BINARY)"
	@if [[ -n "$(PROTO_SOURCE)" ]]; then \
		echo "proto source                : $(PROTO_SOURCE) (explicit)"; \
	elif [[ -r "$(LOCAL_CF_PROTO)" ]]; then \
		echo "proto source                : $(LOCAL_CF_PROTO) (local credentials-fetcher source)"; \
	elif [[ -n "$(CREDENTIALS_FETCHER_VERSION)" ]]; then \
		echo "credentials-fetcher version : $(CREDENTIALS_FETCHER_VERSION)"; \
		echo "credentials-fetcher tag     : $(CREDENTIALS_FETCHER_TAG)"; \
		echo "proto source                : $(PROTO_URL)"; \
	else \
		echo "proto source                : unresolved"; \
	fi

$(BUILD_DIR):
	@mkdir -p "$@"

$(PROTO_DIR): | $(BUILD_DIR)
	@mkdir -p "$@"

proto: | $(PROTO_DIR)
	@rm -f "$(PROTO)"
	@if [[ -n "$(PROTO_SOURCE)" ]]; then \
		echo "Using approved local proto: $(PROTO_SOURCE)"; \
		test -r "$(PROTO_SOURCE)" || { echo "ERROR: PROTO_SOURCE is not readable: $(PROTO_SOURCE)" >&2; exit 1; }; \
		cp "$(PROTO_SOURCE)" "$(PROTO)"; \
	elif [[ -r "$(LOCAL_CF_PROTO)" ]]; then \
		echo "Using credentials-fetcher proto from local source checkout: $(LOCAL_CF_PROTO)"; \
		cp "$(LOCAL_CF_PROTO)" "$(PROTO)"; \
	else \
		if [[ -z "$(CREDENTIALS_FETCHER_VERSION)" ]]; then \
			echo "ERROR: no credentials-fetcher proto source could be resolved." >&2; \
			echo "Expected local source: $(LOCAL_CF_PROTO)" >&2; \
			echo "Or provide PROTO_SOURCE=/path/to/credentialsfetcher.proto" >&2; \
			echo "Or provide CREDENTIALS_FETCHER_VERSION=<version> to fetch the matching tag." >&2; \
			exit 1; \
		fi; \
		echo "Fetching credentials-fetcher proto from tag $(CREDENTIALS_FETCHER_TAG)"; \
		curl --fail --silent --show-error --location "$(PROTO_URL)" --output "$(PROTO)"; \
	fi
	@if [[ -n "$(PROTO_SHA256)" ]]; then \
		echo "$(PROTO_SHA256)  $(PROTO)" | sha256sum --check --strict -; \
	fi

# Validate exactly the AWS protobuf subset embedded in main.go.
# The proto is a build-time source of truth only; it is never shipped at runtime.
validate-proto: proto
	@echo "Validating credentials-fetcher protobuf contract..."
	@grep -Eq '^package[[:space:]]+credentialsfetcher[[:space:]]*;' "$(PROTO)" || \
		{ echo "ERROR: protobuf package is not credentialsfetcher" >&2; exit 1; }
	@awk '\
		/service[[:space:]]+CredentialsFetcherService[[:space:]]*\{/ { in_service=1 } \
		in_service { service = service " " $$0 } \
		in_service && /}/ { \
			gsub(/[[:space:]]+/, " ", service); \
			if (service !~ /rpc AddKerberosLease \(CreateKerberosLeaseRequest\) returns \(CreateKerberosLeaseResponse\);/) exit 11; \
			if (service !~ /rpc DeleteKerberosLease \(DeleteKerberosLeaseRequest\) returns \(DeleteKerberosLeaseResponse\);/) exit 12; \
			exit 0; \
		} \
		END { if (!in_service) exit 13 }' "$(PROTO)" || \
		{ rc=$$?; case $$rc in \
			11) echo "ERROR: AddKerberosLease RPC signature changed" >&2 ;; \
			12) echo "ERROR: DeleteKerberosLease RPC signature changed" >&2 ;; \
			*)  echo "ERROR: CredentialsFetcherService definition missing or unreadable" >&2 ;; \
		esac; exit 1; }
	@sed -n '/^message[[:space:]]\+CreateKerberosLeaseRequest[[:space:]]*{/,/^}/p' "$(PROTO)" | \
		grep -Eq 'repeated[[:space:]]+string[[:space:]]+credspec_contents[[:space:]]*=[[:space:]]*1[[:space:]]*;' || \
		{ echo "ERROR: CreateKerberosLeaseRequest.credspec_contents is no longer repeated string field 1" >&2; exit 1; }
	@sed -n '/^message[[:space:]]\+CreateKerberosLeaseResponse[[:space:]]*{/,/^}/p' "$(PROTO)" | \
		grep -Eq 'string[[:space:]]+lease_id[[:space:]]*=[[:space:]]*1[[:space:]]*;' || \
		{ echo "ERROR: CreateKerberosLeaseResponse.lease_id is no longer string field 1" >&2; exit 1; }
	@sed -n '/^message[[:space:]]\+CreateKerberosLeaseResponse[[:space:]]*{/,/^}/p' "$(PROTO)" | \
		grep -Eq 'repeated[[:space:]]+string[[:space:]]+created_kerberos_file_paths[[:space:]]*=[[:space:]]*2[[:space:]]*;' || \
		{ echo "ERROR: CreateKerberosLeaseResponse.created_kerberos_file_paths is no longer repeated string field 2" >&2; exit 1; }
	@sed -n '/^message[[:space:]]\+DeleteKerberosLeaseRequest[[:space:]]*{/,/^}/p' "$(PROTO)" | \
		grep -Eq 'string[[:space:]]+lease_id[[:space:]]*=[[:space:]]*1[[:space:]]*;' || \
		{ echo "ERROR: DeleteKerberosLeaseRequest.lease_id is no longer string field 1" >&2; exit 1; }
	@sed -n '/^message[[:space:]]\+DeleteKerberosLeaseResponse[[:space:]]*{/,/^}/p' "$(PROTO)" | \
		grep -Eq 'string[[:space:]]+lease_id[[:space:]]*=[[:space:]]*1[[:space:]]*;' || \
		{ echo "ERROR: DeleteKerberosLeaseResponse.lease_id is no longer string field 1" >&2; exit 1; }
	@sed -n '/^message[[:space:]]\+DeleteKerberosLeaseResponse[[:space:]]*{/,/^}/p' "$(PROTO)" | \
		grep -Eq 'repeated[[:space:]]+string[[:space:]]+deleted_kerberos_file_paths[[:space:]]*=[[:space:]]*2[[:space:]]*;' || \
		{ echo "ERROR: DeleteKerberosLeaseResponse.deleted_kerberos_file_paths is no longer repeated string field 2" >&2; exit 1; }
	@echo "credentials-fetcher protobuf contract: OK"

prepare-modules:
	@if [[ ! -f go.sum ]]; then \
		echo "go.sum not present; resolving pinned Go module dependencies..."; \
		go mod tidy; \
	fi

verify-modules: prepare-modules
	@go mod verify

build: validate-proto verify-modules | $(BUILD_DIR)
	@echo "Building $(APP)..."
	@CGO_ENABLED=0 go build \
		-trimpath \
		-ldflags='-s -w' \
		-o "$(BINARY)" .
	@echo "Built: $(BINARY)"

install:
	@test -x "$(BINARY)" || { echo "ERROR: $(BINARY) is missing. Run 'make build' first." >&2; exit 1; }
	@install -d -o root -g root -m 0755 /usr/local/libexec
	@install -o root -g root -m 0750 "$(BINARY)" "$(INSTALL_BIN)"
	@install -o root -g root -m 0644 gmsa-helper@.service "$(INSTALL_UNIT)"
	@echo "Installed: $(INSTALL_BIN)"
	@echo "Installed: $(INSTALL_UNIT)"
	@echo "Run 'sudo systemctl daemon-reload' manually before starting an instance."

uninstall:
	@rm -f "$(INSTALL_BIN)" "$(INSTALL_UNIT)"
	@echo "Removed: $(INSTALL_BIN)"
	@echo "Removed: $(INSTALL_UNIT)"
	@echo "Run 'sudo systemctl daemon-reload' manually."

clean:
	@rm -rf "$(BUILD_DIR)"
