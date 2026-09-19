// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package doctor checks whether a host is ready to run rivorad: bpffs
// mounted, kernel BTF present, kernel new enough for TCX (full-NAT mode's
// reverse path), and the build tools needed to (re)compile the BPF objects.
// Shape mirrors netra's internal/doctor: a Report of Checks plus a Summary,
// human or JSON output, --strict promotes warnings to failures.
package doctor

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusInfo Status = "info"
)

type Check struct {
	Status      Status `json:"status"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type Summary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Info int `json:"info"`
}

type Report struct {
	Hostname      string  `json:"hostname"`
	OS            string  `json:"os"`
	Architecture  string  `json:"architecture"`
	KernelRelease string  `json:"kernelRelease"`
	Checks        []Check `json:"checks"`
	Summary       Summary `json:"summary"`
}

type Options struct {
	Root       string // filesystem root to inspect; "" defaults to "/"
	RequireTCX bool   // fail (not warn) if the kernel predates TCX (6.6)
	Interface  string // optional: also check this interface's XDP driver support
}

func Run(opts Options) Report {
	root := opts.Root
	if root == "" {
		root = "/"
	}

	r := Report{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	if h, err := os.Hostname(); err == nil {
		r.Hostname = h
	}
	r.KernelRelease = kernelRelease(root)

	r.add(checkRoot())
	r.add(checkKernelVersion(r.KernelRelease, opts.RequireTCX))
	r.add(checkBPFFSMounted(root))
	r.add(checkBTF(root))
	r.add(checkTool("clang", "compile bpf/*.c into bpf/*.o (make bpf)"))
	r.add(checkTool("bpftool", "install linux-tools-$(uname -r) — used for map/prog troubleshooting"))
	r.add(checkAPISecurity())
	if opts.Interface != "" {
		r.add(checkInterfaceXDP(opts.Interface))
	}

	for _, c := range r.Checks {
		switch c.Status {
		case StatusPass:
			r.Summary.Pass++
		case StatusWarn:
			r.Summary.Warn++
		case StatusFail:
			r.Summary.Fail++
		case StatusInfo:
			r.Summary.Info++
		}
	}
	return r
}

func (r *Report) add(c Check) { r.Checks = append(r.Checks, c) }

func checkRoot() Check {
	u, err := user.Current()
	if err == nil && u.Uid == "0" {
		return Check{Status: StatusPass, Title: "Privileges", Detail: "running as root"}
	}
	return Check{
		Status: StatusFail, Title: "Privileges",
		Detail:      "not running as root (CAP_BPF/CAP_NET_ADMIN required to load/attach programs)",
		Remediation: "run rivorad/rivora-doctor as root or with sudo",
	}
}

func kernelRelease(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "proc/sys/kernel/osrelease"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// parseKernelVersion returns (major, minor) from a release string like
// "6.8.0-139-generic".
func parseKernelVersion(release string) (int, int, bool) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minorStr := parts[1]
	for i, ch := range minorStr {
		if ch < '0' || ch > '9' {
			minorStr = minorStr[:i]
			break
		}
	}
	minor, err2 := strconv.Atoi(minorStr)
	return major, minor, err1 == nil && err2 == nil
}

func checkKernelVersion(release string, requireTCX bool) Check {
	major, minor, ok := parseKernelVersion(release)
	if !ok {
		return Check{Status: StatusWarn, Title: "Kernel version", Detail: "could not parse kernel release"}
	}
	detail := fmt.Sprintf("running %s", release)
	if major > 6 || (major == 6 && minor >= 6) {
		return Check{Status: StatusPass, Title: "Kernel version", Detail: detail + " (TCX available)"}
	}
	status := StatusWarn
	if requireTCX {
		status = StatusFail
	}
	return Check{
		Status: status, Title: "Kernel version",
		Detail:      detail + " — predates 6.6, no TCX (full-NAT mode's reverse-path program needs TCX)",
		Remediation: "upgrade to Linux 6.6+, or run DSR-mode VIPs only",
	}
}

func checkBPFFSMounted(root string) Check {
	f, err := os.Open(filepath.Join(root, "proc/mounts"))
	if err != nil {
		return Check{Status: StatusWarn, Title: "bpffs", Detail: "could not read /proc/mounts"}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[2] == "bpf" {
			return Check{Status: StatusPass, Title: "bpffs", Detail: "mounted at " + fields[1]}
		}
	}
	return Check{
		Status: StatusFail, Title: "bpffs",
		Detail:      "no bpf filesystem mounted",
		Remediation: "mount -t bpf bpf /sys/fs/bpf",
	}
}

// checkAPISecurity is informational only: whether to secure rivorad's local
// API is the operator's call (v0.1 defaults to loopback-only), not
// something to gate readiness on — same spirit as netra-doctor reporting
// environment facts rather than opinions where there's no single right
// answer.
func checkAPISecurity() Check {
	apiKey := os.Getenv("RIVORA_API_KEY") != ""
	tlsFile := os.Getenv("RIVORA_TLS_CERT") != "" && os.Getenv("RIVORA_TLS_KEY") != ""
	selfSigned := os.Getenv("RIVORA_TLS_SELF_SIGNED") != ""

	readOnly := strings.TrimSpace(os.Getenv("RIVORA_API_READONLY_KEY")) != ""

	authDetail := "RIVORA_API_KEY not set — API is unauthenticated"
	if apiKey {
		authDetail = "RIVORA_API_KEY set — API requires a bearer token"
		if readOnly {
			authDetail += " (a separate RIVORA_API_READONLY_KEY may read but not change anything)"
		}
	}

	// rivorad refuses to start in this state: the admin key is what turns
	// authentication on, so a read-only key alone would protect nothing.
	if readOnly && !apiKey {
		return Check{
			Status: StatusWarn, Title: "API security",
			Detail:      "RIVORA_API_READONLY_KEY is set but RIVORA_API_KEY is not",
			Remediation: "set RIVORA_API_KEY (the admin key that enables authentication), or unset RIVORA_API_READONLY_KEY; rivorad will refuse to start as configured",
		}
	}
	tlsDetail := "no TLS env vars set — API serves plain HTTP"
	switch {
	case tlsFile:
		tlsDetail = "RIVORA_TLS_CERT/RIVORA_TLS_KEY set — API serves HTTPS with that certificate"
	case selfSigned:
		tlsDetail = "RIVORA_TLS_SELF_SIGNED set — API serves HTTPS with an auto-generated self-signed certificate"
	}

	return Check{Status: StatusInfo, Title: "API security", Detail: authDetail + "; " + tlsDetail}
}

func checkBTF(root string) Check {
	path := filepath.Join(root, "sys/kernel/btf/vmlinux")
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return Check{Status: StatusPass, Title: "Kernel BTF", Detail: path}
	}
	return Check{
		Status: StatusInfo, Title: "Kernel BTF",
		Detail: "no /sys/kernel/btf/vmlinux — not required by v0.1 (no CO-RE), but useful for bpftool introspection",
	}
}

func checkTool(name, remediation string) Check {
	if path, err := exec.LookPath(name); err == nil {
		return Check{Status: StatusPass, Title: name, Detail: path}
	}
	return Check{Status: StatusWarn, Title: name, Detail: name + " not found in PATH", Remediation: remediation}
}

func checkInterfaceXDP(iface string) Check {
	out, err := exec.Command("ethtool", "-i", iface).CombinedOutput()
	if err != nil {
		return Check{
			Status: StatusInfo, Title: "Interface " + iface,
			Detail: "ethtool unavailable or interface not found — cannot confirm native XDP driver support; generic/skb mode will still work",
		}
	}
	return Check{Status: StatusInfo, Title: "Interface " + iface, Detail: strings.TrimSpace(string(out))}
}
