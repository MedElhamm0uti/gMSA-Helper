package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	socketPath      = "/var/credentials-fetcher/socket/credentials_fetcher.sock"
	keytabPath      = "/etc/krb5.keytab"
	defaultStateDir = "/run/gmsa-helper"
	krbRoot         = "/var/credentials-fetcher/krbdir"

	addLeaseMethod    = "/credentialsfetcher.CredentialsFetcherService/AddKerberosLease"
	deleteLeaseMethod = "/credentialsfetcher.CredentialsFetcherService/DeleteKerberosLease"
)

var stateDir = defaultStateDir

// These four small types mirror the subset of the AWS credentials-fetcher
// protobuf contract that this helper consumes. The field numbers are part of
// the protobuf wire contract and are pinned here deliberately.
type CreateKerberosLeaseRequest struct {
	CredspecContents []string `protobuf:"bytes,1,rep,name=credspec_contents,json=credspecContents,proto3" json:"credspec_contents,omitempty"`
}

func (m *CreateKerberosLeaseRequest) Reset()         { *m = CreateKerberosLeaseRequest{} }
func (m *CreateKerberosLeaseRequest) String() string { return fmt.Sprintf("%v", m.CredspecContents) }
func (*CreateKerberosLeaseRequest) ProtoMessage()    {}

type CreateKerberosLeaseResponse struct {
	LeaseId                  string   `protobuf:"bytes,1,opt,name=lease_id,json=leaseId,proto3" json:"lease_id,omitempty"`
	CreatedKerberosFilePaths []string `protobuf:"bytes,2,rep,name=created_kerberos_file_paths,json=createdKerberosFilePaths,proto3" json:"created_kerberos_file_paths,omitempty"`
}

func (m *CreateKerberosLeaseResponse) Reset() { *m = CreateKerberosLeaseResponse{} }
func (m *CreateKerberosLeaseResponse) String() string {
	return fmt.Sprintf("lease=%s paths=%v", m.LeaseId, m.CreatedKerberosFilePaths)
}
func (*CreateKerberosLeaseResponse) ProtoMessage() {}

type DeleteKerberosLeaseRequest struct {
	LeaseId string `protobuf:"bytes,1,opt,name=lease_id,json=leaseId,proto3" json:"lease_id,omitempty"`
}

func (m *DeleteKerberosLeaseRequest) Reset()         { *m = DeleteKerberosLeaseRequest{} }
func (m *DeleteKerberosLeaseRequest) String() string { return m.LeaseId }
func (*DeleteKerberosLeaseRequest) ProtoMessage()    {}

type DeleteKerberosLeaseResponse struct {
	LeaseId                  string   `protobuf:"bytes,1,opt,name=lease_id,json=leaseId,proto3" json:"lease_id,omitempty"`
	DeletedKerberosFilePaths []string `protobuf:"bytes,2,rep,name=deleted_kerberos_file_paths,json=deletedKerberosFilePaths,proto3" json:"deleted_kerberos_file_paths,omitempty"`
}

func (m *DeleteKerberosLeaseResponse) Reset() { *m = DeleteKerberosLeaseResponse{} }
func (m *DeleteKerberosLeaseResponse) String() string {
	return fmt.Sprintf("lease=%s paths=%v", m.LeaseId, m.DeletedKerberosFilePaths)
}
func (*DeleteKerberosLeaseResponse) ProtoMessage() {}

type adInfo struct {
	GMSA             string
	Domain           string
	Forest           string
	NetBIOS          string
	DC               string
	SID              string
	GUID             string
	MachinePrincipal string
}

type credSpec struct {
	CmsPlugins            []string              `json:"CmsPlugins"`
	DomainJoinConfig      domainJoinConfig      `json:"DomainJoinConfig"`
	ActiveDirectoryConfig activeDirectoryConfig `json:"ActiveDirectoryConfig"`
}

type domainJoinConfig struct {
	SID                string `json:"Sid"`
	MachineAccountName string `json:"MachineAccountName"`
	GUID               string `json:"Guid"`
	DnsTreeName        string `json:"DnsTreeName"`
	DnsName            string `json:"DnsName"`
	NetBiosName        string `json:"NetBiosName"`
}

type activeDirectoryConfig struct {
	GroupManagedServiceAccounts []gmsaScope `json:"GroupManagedServiceAccounts"`
}

type gmsaScope struct {
	Name  string `json:"Name"`
	Scope string `json:"Scope"`
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: %s start|stop | bind --service <unit.service> --instance <NAME> [--force]", os.Args[0])
	}

	var err error

	switch os.Args[1] {
	case "start", "stop":
		if len(os.Args) != 2 {
			fatalf("usage: %s %s", os.Args[0], os.Args[1])
		}
		if err := configureStateDir(); err != nil {
			fatalf("%v", err)
		}
		if os.Args[1] == "start" {
			err = start()
		} else {
			err = stop()
		}

	case "bind":
		err = bindApplication(os.Args[2:])

	default:
		err = fmt.Errorf("unknown action %q; expected start, stop, or bind", os.Args[1])
	}

	if err != nil {
		fatalf("%v", err)
	}
}

type bindOptions struct {
	Service  string
	Instance string
	Force    bool
}

func bindApplication(args []string) error {
	opts, err := parseBindArgs(args)
	if err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return errors.New("bind must be run as root")
	}

	if err := validateServiceName(opts.Service); err != nil {
		return err
	}
	if err := validateInstanceName(opts.Instance); err != nil {
		return err
	}

	helperTemplate := "gmsa-helper@.service"
	helperService := "gmsa-helper@" + opts.Instance + ".service"
	cachePath := "/run/gmsa-helper-" + opts.Instance + "/krb5cc"

	if _, err := runWithEnv(nil, "systemctl", "cat", opts.Service); err != nil {
		return fmt.Errorf("application service %s was not found: %w", opts.Service, err)
	}
	if _, err := runWithEnv(nil, "systemctl", "cat", helperTemplate); err != nil {
		return fmt.Errorf("gMSA helper template %s was not found: %w", helperTemplate, err)
	}
	if _, err := runWithEnv(nil, "systemctl", "cat", helperService); err != nil {
		return fmt.Errorf("gMSA helper instance %s cannot be resolved: %w", helperService, err)
	}

	dropInDir := filepath.Join("/etc/systemd/system", opts.Service+".d")
	dropInFile := filepath.Join(dropInDir, "gmsa-helper.conf")

	content := fmt.Sprintf(`[Unit]
Requires=%s
After=%s

[Service]
Environment="KRB5CCNAME=FILE:%s"
`, helperService, helperService, cachePath)

	var previous []byte
	hadPrevious := false
	alreadyBound := false

	if current, err := os.ReadFile(dropInFile); err == nil {
		hadPrevious = true
		previous = current

		if string(current) == content {
			alreadyBound = true
		} else if !opts.Force {
			return fmt.Errorf(
				"%s already exists with different content; inspect it or rerun with --force",
				dropInFile,
			)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect existing drop-in %s: %w", dropInFile, err)
	}

	if err := os.MkdirAll(dropInDir, 0755); err != nil {
		return fmt.Errorf("create systemd drop-in directory %s: %w", dropInDir, err)
	}

	if !alreadyBound {
		if err := writeAtomicFile(dropInFile, []byte(content), 0644); err != nil {
			return err
		}
	}

	rollback := func() {
		if hadPrevious {
			_ = writeAtomicFile(dropInFile, previous, 0644)
		} else {
			_ = os.Remove(dropInFile)
			_ = os.Remove(dropInDir)
		}
		_, _ = runWithEnv(nil, "systemctl", "daemon-reload")
	}

	if _, err := runWithEnv(nil, "systemctl", "daemon-reload"); err != nil {
		rollback()
		return fmt.Errorf("systemctl daemon-reload failed; binding rolled back: %w", err)
	}

	requires, err := runWithEnv(nil, "systemctl", "show", opts.Service, "-p", "Requires", "--value")
	if err != nil {
		rollback()
		return fmt.Errorf("verify Requires for %s; binding rolled back: %w", opts.Service, err)
	}
	after, err := runWithEnv(nil, "systemctl", "show", opts.Service, "-p", "After", "--value")
	if err != nil {
		rollback()
		return fmt.Errorf("verify After for %s; binding rolled back: %w", opts.Service, err)
	}
	environment, err := runWithEnv(nil, "systemctl", "show", opts.Service, "-p", "Environment", "--value")
	if err != nil {
		rollback()
		return fmt.Errorf("verify Environment for %s; binding rolled back: %w", opts.Service, err)
	}

	if !containsSystemdWord(requires, helperService) {
		rollback()
		return fmt.Errorf("effective Requires does not contain %s; binding rolled back", helperService)
	}
	if !containsSystemdWord(after, helperService) {
		rollback()
		return fmt.Errorf("effective After does not contain %s; binding rolled back", helperService)
	}

	expectedEnv := "KRB5CCNAME=FILE:" + cachePath
	if !strings.Contains(environment, expectedEnv) {
		rollback()
		return fmt.Errorf("effective environment does not contain %s; binding rolled back", expectedEnv)
	}

	if alreadyBound {
		fmt.Printf("%s is already bound to %s\n", opts.Service, helperService)
	} else {
		fmt.Printf("Bound %s to %s\n", opts.Service, helperService)
	}
	fmt.Printf("Drop-in  : %s\n", dropInFile)
	fmt.Printf("Cache    : %s\n", cachePath)
	fmt.Println("Application service was not restarted.")

	return nil
}

func parseBindArgs(args []string) (bindOptions, error) {
	var opts bindOptions

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--service":
			if i+1 >= len(args) {
				return opts, errors.New("--service requires a value")
			}
			i++
			opts.Service = strings.TrimSpace(args[i])

		case "--instance":
			if i+1 >= len(args) {
				return opts, errors.New("--instance requires a value")
			}
			i++
			opts.Instance = strings.TrimSpace(args[i])

		case "--force":
			opts.Force = true

		case "-h", "--help":
			return opts, errors.New("usage: gmsa-helper bind --service <unit.service> --instance <NAME> [--force]")

		default:
			return opts, fmt.Errorf("unknown bind option %q", args[i])
		}
	}

	if opts.Service == "" {
		return opts, errors.New("bind requires --service <unit.service>")
	}
	if opts.Instance == "" {
		return opts, errors.New("bind requires --instance <NAME>")
	}

	return opts, nil
}

func validateServiceName(service string) error {
	if strings.HasPrefix(service, "-") {
		return fmt.Errorf("unsafe systemd service name: %s", service)
	}
	if !strings.HasSuffix(service, ".service") {
		return fmt.Errorf("service must end in .service: %s", service)
	}
	if strings.Contains(service, "/") || strings.Contains(service, "..") {
		return fmt.Errorf("unsafe systemd service name: %s", service)
	}
	for _, r := range service {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '@' || r == ':' {
			continue
		}
		return fmt.Errorf("unsupported character %q in systemd service name %s", r, service)
	}
	return nil
}

func validateInstanceName(instance string) error {
	if strings.HasPrefix(instance, "-") {
		return fmt.Errorf("unsafe gMSA helper instance name: %s", instance)
	}
	if instance == "" || instance == "." || instance == ".." {
		return fmt.Errorf("invalid gMSA helper instance name: %q", instance)
	}
	if strings.Contains(instance, "/") || strings.Contains(instance, "..") {
		return fmt.Errorf("unsafe gMSA helper instance name: %s", instance)
	}
	for _, r := range instance {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("unsupported character %q in gMSA helper instance name %s", r, instance)
	}
	return nil
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".gmsa-helper.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary file %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}

	cleanup = false
	return nil
}

func containsSystemdWord(text, wanted string) bool {
	for _, field := range strings.Fields(text) {
		if field == wanted {
			return true
		}
	}
	return false
}

func configureStateDir() error {
	configured := strings.TrimSpace(os.Getenv("GMSA_STATE_DIR"))
	if configured == "" {
		stateDir = defaultStateDir
		return nil
	}

	if !filepath.IsAbs(configured) {
		return fmt.Errorf("GMSA_STATE_DIR must be an absolute path: %s", configured)
	}

	stateDir = filepath.Clean(configured)
	return nil
}

func start() error {
	gmsa := strings.TrimSpace(os.Getenv("GMSA_NAME"))
	if gmsa == "" {
		return errors.New("GMSA_NAME is not configured")
	}

	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	leaseFile := filepath.Join(stateDir, "lease-id")
	if _, err := os.Stat(leaseFile); err == nil {
		return fmt.Errorf("lease state already exists at %s; stop the unit before creating another lease", leaseFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect lease state: %w", err)
	}

	ad, err := discoverAD(gmsa)
	if err != nil {
		return err
	}

	cs := credSpec{
		CmsPlugins: []string{"ActiveDirectory"},
		DomainJoinConfig: domainJoinConfig{
			SID:                ad.SID,
			MachineAccountName: ad.GMSA,
			GUID:               ad.GUID,
			DnsTreeName:        ad.Forest,
			DnsName:            ad.Domain,
			NetBiosName:        ad.NetBIOS,
		},
		ActiveDirectoryConfig: activeDirectoryConfig{
			GroupManagedServiceAccounts: []gmsaScope{
				{Name: ad.GMSA, Scope: ad.Domain},
				{Name: ad.GMSA, Scope: ad.NetBIOS},
			},
		},
	}

	credspecJSON, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("marshal CredentialSpec: %w", err)
	}

	conn, err := dialCredentialsFetcher()
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request := &CreateKerberosLeaseRequest{
		CredspecContents: []string{string(credspecJSON)},
	}
	response := &CreateKerberosLeaseResponse{}

	if err := conn.Invoke(ctx, addLeaseMethod, request, response); err != nil {
		return fmt.Errorf("AddKerberosLease: %w", err)
	}
	if response.LeaseId == "" {
		return errors.New("AddKerberosLease returned an empty lease ID")
	}
	if len(response.CreatedKerberosFilePaths) == 0 {
		_ = deleteLease(conn, response.LeaseId)
		return errors.New("AddKerberosLease returned no Kerberos paths")
	}

	// Once AWS has created a lease, every remaining startup step is treated as
	// a transaction. If anything below fails, remove the lease immediately so
	// we do not leave an orphan behind.
	committed := false
	defer func() {
		if !committed {
			_ = deleteLease(conn, response.LeaseId)
			cleanupRuntimeState()
		}
	}()

	cache, err := locateKerberosCache(response.LeaseId, response.CreatedKerberosFilePaths)
	if err != nil {
		return err
	}

	if _, err := runWithEnv(nil, "klist", "-c", cache); err != nil {
		return fmt.Errorf("validate gMSA Kerberos cache %s: %w", cache, err)
	}

	if err := writePrivateFile(filepath.Join(stateDir, "lease-id"), response.LeaseId+"\n"); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(stateDir, "kerberos-paths"), strings.Join(response.CreatedKerberosFilePaths, "\n")+"\n"); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(stateDir, "kerberos-cache"), cache+"\n"); err != nil {
		return err
	}

	stableCache := filepath.Join(stateDir, "krb5cc")
	if err := os.Remove(stableCache); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old stable cache link: %w", err)
	}
	if err := os.Symlink(cache, stableCache); err != nil {
		return fmt.Errorf("create stable cache symlink: %w", err)
	}

	committed = true

	fmt.Println("gMSA initialized successfully")
	fmt.Printf("gMSA              : %s\n", ad.GMSA)
	fmt.Printf("Domain            : %s\n", ad.Domain)
	fmt.Printf("Forest            : %s\n", ad.Forest)
	fmt.Printf("NetBIOS           : %s\n", ad.NetBIOS)
	fmt.Printf("DC                : %s\n", ad.DC)
	fmt.Printf("Machine principal : %s\n", ad.MachinePrincipal)
	fmt.Printf("Domain SID        : %s\n", ad.SID)
	fmt.Printf("Domain GUID       : %s\n", ad.GUID)
	fmt.Printf("Lease ID          : %s\n", response.LeaseId)
	fmt.Printf("Kerberos cache    : %s\n", cache)
	fmt.Printf("Stable cache      : %s\n", stableCache)

	return nil
}

func stop() error {
	leaseFile := filepath.Join(stateDir, "lease-id")
	data, err := os.ReadFile(leaseFile)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Printf("No gMSA lease recorded in %s\n", stateDir)
		cleanupRuntimeState()
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lease ID: %w", err)
	}

	leaseID := strings.TrimSpace(string(data))
	if leaseID == "" {
		return errors.New("recorded lease ID is empty")
	}

	conn, err := dialCredentialsFetcher()
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := deleteLease(conn, leaseID); err != nil {
		// Preserve runtime state on failure so an administrator can retry the
		// cleanup instead of losing the lease identifier.
		return err
	}

	cleanupRuntimeState()
	fmt.Printf("Deleted gMSA lease %s\n", leaseID)
	return nil
}

func deleteLease(conn *grpc.ClientConn, leaseID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	request := &DeleteKerberosLeaseRequest{LeaseId: leaseID}
	response := &DeleteKerberosLeaseResponse{}
	if err := conn.Invoke(ctx, deleteLeaseMethod, request, response); err != nil {
		return fmt.Errorf("DeleteKerberosLease(%s): %w", leaseID, err)
	}
	return nil
}

func dialCredentialsFetcher() (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}

	conn, err := grpc.DialContext(
		ctx,
		"passthrough:///credentials-fetcher",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to credentials-fetcher socket %s: %w", socketPath, err)
	}
	return conn, nil
}

func discoverAD(gmsa string) (*adInfo, error) {
	realmsText, err := runWithEnv(nil, "realm", "list", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("discover joined realm: %w", err)
	}
	realms := nonEmptyLines(realmsText)
	if len(realms) != 1 {
		return nil, fmt.Errorf("expected exactly one joined realm, found %v", realms)
	}
	joinedDomain := realms[0]

	cEnv := append(os.Environ(), "LC_ALL=C")
	adcliText, err := runWithEnv(cEnv, "adcli", "info", joinedDomain)
	if err != nil {
		return nil, fmt.Errorf("adcli discovery for %s: %w", joinedDomain, err)
	}
	info := parseEquals(adcliText)

	dnsDomain := info["domain-name"]
	netbios := info["domain-short"]
	forest := info["domain-forest"]
	dc := info["domain-controller"]
	if dnsDomain == "" || netbios == "" || forest == "" || dc == "" {
		return nil, fmt.Errorf("adcli did not return all required fields: domain-name=%q domain-short=%q domain-forest=%q domain-controller=%q", dnsDomain, netbios, forest, dc)
	}

	realm := strings.ToUpper(dnsDomain)
	machinePrincipal, err := findMachinePrincipal(realm)
	if err != nil {
		return nil, err
	}

	discoveryCache := filepath.Join(stateDir, "discovery.ccache")
	_ = os.Remove(discoveryCache)
	krbEnv := append(os.Environ(), "KRB5CCNAME=FILE:"+discoveryCache)

	if _, err := runWithEnv(krbEnv, "kinit", "-k", "-t", keytabPath, machinePrincipal); err != nil {
		return nil, fmt.Errorf("obtain machine Kerberos TGT: %w", err)
	}
	defer func() {
		_, _ = runWithEnv(krbEnv, "kdestroy", "-c", discoveryCache)
		_ = os.Remove(discoveryCache)
	}()

	ldapBase := []string{"-LLL", "-o", "ldif-wrap=no", "-Y", "GSSAPI", "-H", "ldap://" + dc}

	rootArgs := append(copyStrings(ldapBase), "-b", "", "-s", "base", "defaultNamingContext")
	rootDSE, err := runWithEnv(krbEnv, "ldapsearch", rootArgs...)
	if err != nil {
		return nil, fmt.Errorf("query LDAP RootDSE: %w", err)
	}
	namingContext, _, err := ldifValue(rootDSE, "defaultNamingContext")
	if err != nil {
		return nil, err
	}

	domainArgs := append(copyStrings(ldapBase), "-b", namingContext, "-s", "base", "objectSid", "objectGUID")
	domainObject, err := runWithEnv(krbEnv, "ldapsearch", domainArgs...)
	if err != nil {
		return nil, fmt.Errorf("query AD domain object: %w", err)
	}

	_, sidBytes, err := ldifValue(domainObject, "objectSid")
	if err != nil {
		return nil, err
	}
	if sidBytes == nil {
		return nil, errors.New("objectSid was not returned as base64/binary LDIF data")
	}
	_, guidBytes, err := ldifValue(domainObject, "objectGUID")
	if err != nil {
		return nil, err
	}
	if guidBytes == nil {
		return nil, errors.New("objectGUID was not returned as base64/binary LDIF data")
	}

	domainSID, err := sidFromBytes(sidBytes)
	if err != nil {
		return nil, err
	}
	domainGUID, err := guidFromADBytes(guidBytes)
	if err != nil {
		return nil, err
	}

	wantedSAM := strings.TrimSuffix(gmsa, "$") + "$"
	filter := "(&(objectClass=msDS-GroupManagedServiceAccount)(sAMAccountName=" + ldapFilterEscape(wantedSAM) + "))"
	gmsaArgs := append(copyStrings(ldapBase), "-b", namingContext, "-s", "sub", filter, "sAMAccountName")
	gmsaResult, err := runWithEnv(krbEnv, "ldapsearch", gmsaArgs...)
	if err != nil {
		return nil, fmt.Errorf("query gMSA %s: %w", wantedSAM, err)
	}
	foundSAM, foundSAMBytes, err := ldifValue(gmsaResult, "sAMAccountName")
	if err != nil {
		return nil, err
	}
	if foundSAMBytes != nil {
		foundSAM = string(foundSAMBytes)
	}
	if !strings.EqualFold(foundSAM, wantedSAM) {
		return nil, fmt.Errorf("unexpected gMSA returned by AD: %q", foundSAM)
	}

	return &adInfo{
		GMSA:             strings.TrimSuffix(foundSAM, "$"),
		Domain:           dnsDomain,
		Forest:           forest,
		NetBIOS:          netbios,
		DC:               dc,
		SID:              domainSID,
		GUID:             domainGUID,
		MachinePrincipal: machinePrincipal,
	}, nil
}

func findMachinePrincipal(realm string) (string, error) {
	text, err := runWithEnv(nil, "klist", "-k", keytabPath)
	if err != nil {
		return "", fmt.Errorf("inspect machine keytab %s: %w", keytabPath, err)
	}

	suffix := "$@" + strings.ToUpper(realm)
	seen := map[string]bool{}
	var candidates []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		principal := fields[len(fields)-1]
		if strings.HasSuffix(strings.ToUpper(principal), suffix) && !seen[principal] {
			seen[principal] = true
			candidates = append(candidates, principal)
		}
	}

	if len(candidates) != 1 {
		return "", fmt.Errorf("could not uniquely identify machine-account principal in %s: %v", keytabPath, candidates)
	}
	return candidates[0], nil
}

func locateKerberosCache(leaseID string, returnedPaths []string) (string, error) {
	seen := map[string]bool{}
	var candidates []string

	addPath := func(path string) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		if !seen[resolved] {
			seen[resolved] = true
			candidates = append(candidates, path)
		}
	}

	search := func(root string) {
		info, err := os.Stat(root)
		if err != nil {
			return
		}
		if !info.IsDir() {
			if strings.HasPrefix(filepath.Base(root), "krb5cc") {
				addPath(root)
			}
			return
		}
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "krb5cc") {
				addPath(path)
			}
			return nil
		})
	}

	for _, p := range returnedPaths {
		search(p)
	}
	search(filepath.Join(krbRoot, leaseID))

	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", fmt.Errorf("unable to locate Kerberos cache for lease %s", leaseID)
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("multiple Kerberos caches found for lease %s: %v", leaseID, candidates)
	}
	return candidates[0], nil
}

func runWithEnv(env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
	}
	return text, nil
}

func parseEquals(text string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		result[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return result
}

func ldifValue(text, attribute string) (string, []byte, error) {
	plainPrefix := attribute + ": "
	encodedPrefix := attribute + ":: "
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, encodedPrefix) {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, encodedPrefix)))
			if err != nil {
				return "", nil, fmt.Errorf("decode LDAP attribute %s: %w", attribute, err)
			}
			return "", decoded, nil
		}
		if strings.HasPrefix(line, plainPrefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, plainPrefix)), nil, nil
		}
	}
	return "", nil, fmt.Errorf("LDAP attribute not found: %s", attribute)
}

func sidFromBytes(data []byte) (string, error) {
	if len(data) < 8 {
		return "", errors.New("invalid binary SID: too short")
	}
	revision := data[0]
	count := int(data[1])
	if len(data) < 8+4*count {
		return "", errors.New("invalid binary SID: truncated sub-authorities")
	}

	var authority uint64
	for _, b := range data[2:8] {
		authority = (authority << 8) | uint64(b)
	}

	parts := []string{fmt.Sprintf("S-%d-%d", revision, authority)}
	offset := 8
	for i := 0; i < count; i++ {
		parts = append(parts, fmt.Sprintf("%d", binary.LittleEndian.Uint32(data[offset:offset+4])))
		offset += 4
	}
	return strings.Join(parts, "-"), nil
}

func guidFromADBytes(data []byte) (string, error) {
	if len(data) != 16 {
		return "", fmt.Errorf("invalid objectGUID length: %d", len(data))
	}
	return fmt.Sprintf(
		"%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		binary.LittleEndian.Uint32(data[0:4]),
		binary.LittleEndian.Uint16(data[4:6]),
		binary.LittleEndian.Uint16(data[6:8]),
		data[8], data[9], data[10], data[11], data[12], data[13], data[14], data[15],
	), nil
}

func ldapFilterEscape(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\5c",
		"*", "\\2a",
		"(", "\\28",
		")", "\\29",
		"\x00", "\\00",
	)
	return replacer.Replace(value)
}

func writePrivateFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func cleanupRuntimeState() {
	for _, name := range []string{"lease-id", "kerberos-paths", "kerberos-cache", "krb5cc", "discovery.ccache"} {
		_ = os.Remove(filepath.Join(stateDir, name))
	}
}

func nonEmptyLines(text string) []string {
	var result []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func copyStrings(values []string) []string {
	return append([]string(nil), values...)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}
