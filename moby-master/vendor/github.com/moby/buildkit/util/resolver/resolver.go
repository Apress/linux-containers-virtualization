package resolver

import (
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/moby/buildkit/cmd/buildkitd/config"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/util/tracing"
	"github.com/pkg/errors"
)

func fillInsecureOpts(host string, c config.RegistryConfig, h *docker.RegistryHost) error {
	tc, err := loadTLSConfig(c)
	if err != nil {
		return err
	}

	if c.PlainHTTP != nil && *c.PlainHTTP {
		h.Scheme = "http"
	} else if c.Insecure != nil && *c.Insecure {
		tc.InsecureSkipVerify = true
	} else if c.PlainHTTP == nil {
		if ok, _ := docker.MatchLocalhost(host); ok {
			h.Scheme = "http"
		}
	}

	transport := newDefaultTransport()
	transport.TLSClientConfig = tc

	h.Client = &http.Client{
		Transport: tracing.NewTransport(transport),
	}
	return nil
}

func loadTLSConfig(c config.RegistryConfig) (*tls.Config, error) {
	for _, d := range c.TLSConfigDir {
		fs, err := ioutil.ReadDir(d)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return nil, errors.WithStack(err)
		}
		for _, f := range fs {
			if strings.HasSuffix(f.Name(), ".crt") {
				c.RootCAs = append(c.RootCAs, filepath.Join(d, f.Name()))
			}
			if strings.HasSuffix(f.Name(), ".cert") {
				c.KeyPairs = append(c.KeyPairs, config.TLSKeyPair{
					Certificate: filepath.Join(d, f.Name()),
					Key:         filepath.Join(d, strings.TrimSuffix(f.Name(), ".cert")+".key"),
				})
			}
		}
	}

	tc := &tls.Config{}
	if len(c.RootCAs) > 0 {
		systemPool, err := x509.SystemCertPool()
		if err != nil {
			if runtime.GOOS == "windows" {
				systemPool = x509.NewCertPool()
			} else {
				return nil, errors.Wrapf(err, "unable to get system cert pool")
			}
		}
		tc.RootCAs = systemPool
	}

	for _, p := range c.RootCAs {
		dt, err := ioutil.ReadFile(p)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read %s", p)
		}
		tc.RootCAs.AppendCertsFromPEM(dt)
	}

	for _, kp := range c.KeyPairs {
		cert, err := tls.LoadX509KeyPair(kp.Certificate, kp.Key)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load keypair for %s", kp.Certificate)
		}
		tc.Certificates = append(tc.Certificates, cert)
	}
	return tc, nil
}

func NewRegistryConfig(m map[string]config.RegistryConfig) docker.RegistryHosts {
	return docker.Registries(
		func(host string) ([]docker.RegistryHost, error) {
			c, ok := m[host]
			if !ok {
				return nil, nil
			}

			var out []docker.RegistryHost

			for _, mirror := range c.Mirrors {
				h := docker.RegistryHost{
					Scheme:       "https",
					Client:       newDefaultClient(),
					Host:         mirror,
					Path:         "/v2",
					Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve,
				}

				if err := fillInsecureOpts(mirror, m[mirror], &h); err != nil {
					return nil, err
				}

				out = append(out, h)
			}

			if host == "docker.io" {
				host = "registry-1.docker.io"
			}

			h := docker.RegistryHost{
				Scheme:       "https",
				Client:       newDefaultClient(),
				Host:         host,
				Path:         "/v2",
				Capabilities: docker.HostCapabilityPush | docker.HostCapabilityPull | docker.HostCapabilityResolve,
			}

			if err := fillInsecureOpts(host, c, &h); err != nil {
				return nil, err
			}

			out = append(out, h)
			return out, nil
		},
		docker.ConfigureDefaultRegistries(
			docker.WithClient(newDefaultClient()),
			docker.WithPlainHTTP(docker.MatchLocalhost),
		),
	)
}

type SessionAuthenticator struct {
	sm      *session.Manager
	groups  []session.Group
	mu      sync.RWMutex
	cache   map[string]credentials
	cacheMu sync.RWMutex
}

type credentials struct {
	user    string
	secret  string
	created time.Time
}

func NewSessionAuthenticator(sm *session.Manager, g session.Group) *SessionAuthenticator {
	return &SessionAuthenticator{sm: sm, groups: []session.Group{g}, cache: map[string]credentials{}}
}

func (a *SessionAuthenticator) credentials(h string) (string, string, error) {
	const credentialsTimeout = time.Minute

	a.cacheMu.RLock()
	c, ok := a.cache[h]
	if ok && time.Since(c.created) < credentialsTimeout {
		a.cacheMu.RUnlock()
		return c.user, c.secret, nil
	}
	a.cacheMu.RUnlock()

	a.mu.RLock()
	defer a.mu.RUnlock()

	var err error
	for i := len(a.groups) - 1; i >= 0; i-- {
		var user, secret string
		user, secret, err = auth.CredentialsFunc(a.sm, a.groups[i])(h)
		if err != nil {
			continue
		}
		a.cacheMu.Lock()
		a.cache[h] = credentials{user: user, secret: secret, created: time.Now()}
		a.cacheMu.Unlock()
		return user, secret, nil
	}
	return "", "", err
}

func (a *SessionAuthenticator) AddSession(g session.Group) {
	a.mu.Lock()
	a.groups = append(a.groups, g)
	a.mu.Unlock()
}

func New(hosts docker.RegistryHosts, auth *SessionAuthenticator) remotes.Resolver {
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: hostsWithCredentials(hosts, auth),
	})
}

func hostsWithCredentials(hosts docker.RegistryHosts, auth *SessionAuthenticator) docker.RegistryHosts {
	if hosts == nil {
		return nil
	}
	return func(domain string) ([]docker.RegistryHost, error) {
		res, err := hosts(domain)
		if err != nil {
			return nil, err
		}
		if len(res) == 0 {
			return nil, nil
		}

		a := docker.NewDockerAuthorizer(
			docker.WithAuthClient(res[0].Client),
			docker.WithAuthCreds(auth.credentials),
		)
		for i := range res {
			res[i].Authorizer = a
		}
		return res, nil
	}
}

func newDefaultClient() *http.Client {
	return &http.Client{
		Transport: newDefaultTransport(),
	}
}

// newDefaultTransport is for pull or push client
//
// NOTE: For push, there must disable http2 for https because the flow control
// will limit data transfer. The net/http package doesn't provide http2 tunable
// settings which limits push performance.
//
// REF: https://github.com/golang/go/issues/14077
func newDefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		DisableKeepAlives:     true,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
	}
}
