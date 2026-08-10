package dockerprov

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// engine is a minimal Docker Engine API client.
//
// The official client library pulls in a very large dependency tree for the
// six endpoints this orchestrator uses, so the wire protocol is spoken
// directly here. Supported endpoints:
//
//	unix:///var/run/docker.sock   local daemon
//	tcp://host:2376               remote daemon (with TLS)
//	ssh://user@host               remote daemon over SSH, no exposed port
type engine struct {
	http    *http.Client
	baseURL string // scheme://host used for URL construction
	host    string // original host string, for error messages
	auth    string // X-Registry-Auth header value, if configured
}

// TLSOpts configures mutual TLS for a tcp:// daemon.
type TLSOpts struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	SkipVerify bool
}

func newEngine(host string, tlsOpts *TLSOpts) (*engine, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	e := &engine{host: host}

	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		e.baseURL = "http://docker"
		e.http = &http.Client{
			Timeout: 2 * time.Minute,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		}

	case strings.HasPrefix(host, "ssh://"):
		// Reuse the docker CLI's own remote transport: ssh in, then have the
		// remote docker binary proxy the daemon socket over stdio. No daemon
		// port is exposed and existing SSH keys and config are honoured.
		u, err := url.Parse(host)
		if err != nil {
			return nil, fmt.Errorf("parse docker host %q: %w", host, err)
		}
		e.baseURL = "http://docker"
		e.http = &http.Client{
			Timeout: 2 * time.Minute,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialSSH(ctx, u)
				},
			},
		}

	case strings.HasPrefix(host, "tcp://"), strings.HasPrefix(host, "http://"), strings.HasPrefix(host, "https://"):
		addr := host
		for _, p := range []string{"tcp://", "http://", "https://"} {
			addr = strings.TrimPrefix(addr, p)
		}
		tr := &http.Transport{}
		scheme := "http"
		if tlsOpts != nil {
			cfg, err := buildTLS(tlsOpts)
			if err != nil {
				return nil, err
			}
			tr.TLSClientConfig = cfg
			scheme = "https"
		} else if strings.HasPrefix(host, "https://") {
			scheme = "https"
		}
		e.baseURL = scheme + "://" + addr
		e.http = &http.Client{Timeout: 2 * time.Minute, Transport: tr}

	case strings.HasPrefix(host, "npipe://"):
		return nil, fmt.Errorf("docker host %q: Windows named pipes are not supported. "+
			"Either run the orchestrator elsewhere and point this pool at ssh://user@windows-host, "+
			"or enable \"Expose daemon on tcp://localhost:2375\" in Docker Desktop and use tcp://localhost:2375", host)

	default:
		return nil, fmt.Errorf("unsupported docker host %q (want unix://, tcp://, or ssh://)", host)
	}

	return e, nil
}

// dialSSH opens a connection to a remote Docker daemon through SSH.
func dialSSH(ctx context.Context, u *url.URL) (net.Conn, error) {
	args := []string{
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
	}
	if u.Port() != "" {
		args = append(args, "-p", u.Port())
	}
	target := u.Hostname()
	if u.User != nil && u.User.Username() != "" {
		target = u.User.Username() + "@" + target
	}
	args = append(args, target, "--", "docker", "system", "dial-stdio")

	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh to %s: %w", target, err)
	}

	return &sshConn{cmd: cmd, r: stdout, w: stdin, stderr: &stderr}, nil
}

// sshConn adapts an ssh subprocess's stdio to net.Conn.
type sshConn struct {
	cmd    *exec.Cmd
	r      io.ReadCloser
	w      io.WriteCloser
	stderr *bytes.Buffer
}

func (c *sshConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *sshConn) Write(b []byte) (int, error) { return c.w.Write(b) }

func (c *sshConn) Close() error {
	c.w.Close()
	c.r.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

func (c *sshConn) LocalAddr() net.Addr              { return sshAddr{} }
func (c *sshConn) RemoteAddr() net.Addr             { return sshAddr{} }
func (c *sshConn) SetDeadline(time.Time) error      { return nil }
func (c *sshConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshConn) SetWriteDeadline(time.Time) error { return nil }

type sshAddr struct{}

func (sshAddr) Network() string { return "ssh" }
func (sshAddr) String() string  { return "ssh" }

func buildTLS(o *TLSOpts) (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: o.SkipVerify} //nolint:gosec // opt-in
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read docker CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("docker CA %s contains no certificates", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	if o.CertFile != "" || o.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load docker client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// setRegistryAuth configures credentials for pulling private images.
func (e *engine) setRegistryAuth(username, password, server string) {
	if username == "" && password == "" {
		return
	}
	if server == "" {
		server = "https://index.docker.io/v1/"
	}
	blob, _ := json.Marshal(map[string]string{
		"username":      username,
		"password":      password,
		"serveraddress": server,
	})
	e.auth = base64.URLEncoding.EncodeToString(blob)
}

// do performs a request against the daemon.
func (e *engine) do(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", e.host, err)
	}
	return resp, nil
}

// call performs a request and decodes a JSON response into out.
func (e *engine) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := e.do(ctx, method, path, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &engineError{Status: resp.StatusCode, Message: readDockerError(resp.Body), Path: path}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type engineError struct {
	Status  int
	Message string
	Path    string
}

func (e *engineError) Error() string {
	return fmt.Sprintf("docker %s: %d %s", e.Path, e.Status, e.Message)
}

func isNotFound(err error) bool {
	var ee *engineError
	return errors.As(err, &ee) && ee.Status == http.StatusNotFound
}

func readDockerError(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &e); err == nil && e.Message != "" {
		return e.Message
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "(no message)"
	}
	return s
}

// --- API surface used by the provider ---------------------------------------

type versionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

func (e *engine) version(ctx context.Context) (*versionInfo, error) {
	var v versionInfo
	if err := e.call(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Image  string            `json:"Image"`
}

func (e *engine) listContainers(ctx context.Context, labelFilters []string) ([]containerSummary, error) {
	filters := map[string][]string{"label": labelFilters}
	blob, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}
	path := "/containers/json?all=true&filters=" + url.QueryEscape(string(blob))
	var out []containerSummary
	if err := e.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type containerInspect struct {
	ID    string `json:"Id"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		OOMKilled  bool   `json:"OOMKilled"`
		Error      string `json:"Error"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (e *engine) inspect(ctx context.Context, id string) (*containerInspect, error) {
	var out containerInspect
	if err := e.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type createRequest struct {
	Image      string            `json:"Image"`
	Env        []string          `json:"Env,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	Hostname   string            `json:"Hostname,omitempty"`
	StopSignal string            `json:"StopSignal,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	AutoRemove    bool              `json:"AutoRemove"`
	Privileged    bool              `json:"Privileged,omitempty"`
	NetworkMode   string            `json:"NetworkMode,omitempty"`
	Binds         []string          `json:"Binds,omitempty"`
	Tmpfs         map[string]string `json:"Tmpfs,omitempty"`
	ShmSize       int64             `json:"ShmSize,omitempty"`
	Memory        int64             `json:"Memory,omitempty"`
	NanoCPUs      int64             `json:"NanoCpus,omitempty"`
	Isolation     string            `json:"Isolation,omitempty"`
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
	LogConfig struct {
		Type   string            `json:"Type,omitempty"`
		Config map[string]string `json:"Config,omitempty"`
	} `json:"LogConfig"`
}

type createResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

func (e *engine) createContainer(ctx context.Context, name string, req createRequest, platform string) (*createResponse, error) {
	q := url.Values{}
	q.Set("name", name)
	if platform != "" {
		q.Set("platform", platform)
	}
	var out createResponse
	if err := e.call(ctx, http.MethodPost, "/containers/create?"+q.Encode(), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *engine) startContainer(ctx context.Context, id string) error {
	return e.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
}

// removeContainer deletes the container and its anonymous volumes. The v=true
// is what guarantees the job workspace does not survive: without it, anonymous
// volumes pile up on disk indefinitely.
func (e *engine) removeContainer(ctx context.Context, id string) error {
	err := e.call(ctx, http.MethodDelete, "/containers/"+id+"?force=true&v=true", nil, nil)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

func (e *engine) imageExists(ctx context.Context, image string) (bool, error) {
	err := e.call(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// pullImage pulls an image, draining the progress stream to completion. The
// daemon reports mid-stream errors in the body rather than the status code, so
// the stream has to be inspected rather than discarded.
func (e *engine) pullImage(ctx context.Context, image, platform string) error {
	name, tag := splitImageTag(image)
	q := url.Values{}
	q.Set("fromImage", name)
	if tag != "" {
		q.Set("tag", tag)
	}
	if platform != "" {
		q.Set("platform", platform)
	}

	headers := map[string]string{}
	if e.auth != "" {
		headers["X-Registry-Auth"] = e.auth
	}

	resp, err := e.do(ctx, http.MethodPost, "/images/create?"+q.Encode(), nil, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &engineError{Status: resp.StatusCode, Message: readDockerError(resp.Body), Path: "/images/create"}
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("pull %s: %w", image, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", image, msg.Error)
		}
	}
}

// logs returns the tail of a container's output, de-multiplexing Docker's
// stdout/stderr stream framing.
func (e *engine) logs(ctx context.Context, id string, lines int) (string, error) {
	if lines <= 0 {
		lines = 100
	}
	path := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&tail=%d", id, lines)
	resp, err := e.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &engineError{Status: resp.StatusCode, Message: readDockerError(resp.Body), Path: path}
	}
	return demuxLogs(io.LimitReader(resp.Body, 4<<20))
}

// demuxLogs strips Docker's 8-byte stream headers. Containers started without
// a TTY (which is all of ours) return frames, not raw text.
func demuxLogs(r io.Reader) (string, error) {
	var out strings.Builder
	br := bufio.NewReader(r)
	header := make([]byte, 8)

	for {
		if _, err := io.ReadFull(br, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out.String(), nil
			}
			return out.String(), err
		}
		// A frame header always starts with stream type 0-2 followed by three
		// zero bytes. Anything else means the daemon sent unframed output.
		if header[0] > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			out.Write(header)
			rest, _ := io.ReadAll(br)
			out.Write(rest)
			return out.String(), nil
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size > 1<<20 {
			size = 1 << 20
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			out.Write(buf)
			return out.String(), nil
		}
		out.Write(buf)
	}
}

func splitImageTag(image string) (string, string) {
	if i := strings.LastIndex(image, "@"); i > 0 {
		return image[:i], image[i+1:] // digest reference
	}
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return image, "latest"
	}
	// A colon inside the registry host (registry:5000/img) is not a tag.
	if strings.Contains(image[i+1:], "/") {
		return image, "latest"
	}
	return image[:i], image[i+1:]
}

// parseBytes converts "4g", "512m", "1024" into bytes.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "gb"), strings.HasSuffix(s, "g"):
		mult = 1 << 30
		s = strings.TrimSuffix(strings.TrimSuffix(s, "gb"), "g")
	case strings.HasSuffix(s, "mb"), strings.HasSuffix(s, "m"):
		mult = 1 << 20
		s = strings.TrimSuffix(strings.TrimSuffix(s, "mb"), "m")
	case strings.HasSuffix(s, "kb"), strings.HasSuffix(s, "k"):
		mult = 1 << 10
		s = strings.TrimSuffix(strings.TrimSuffix(s, "kb"), "k")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return int64(n * float64(mult)), nil
}
