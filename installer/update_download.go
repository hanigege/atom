package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var updateMirrors = []string{"https://gh-proxy.com/", "https://ghfast.top/", "https://down.mxw.xx.kg/", "https://gh.noki.icu/"}

type downloadSource struct {
	name   string
	proxy  *url.URL
	prefix string
}

func updateSources(port int) []downloadSource {
	var sources []downloadSource
	if port > 0 && port <= 65535 {
		proxy := &url.URL{Scheme: "socks5h", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
		sources = append(sources, downloadSource{name: "本机 mixed · " + proxy.Host, proxy: proxy})
	}
	sources = append(sources, downloadSource{name: "GitHub 直连"})
	for _, prefix := range updateMirrors {
		sources = append(sources, downloadSource{name: strings.TrimPrefix(strings.TrimSuffix(prefix, "/"), "https://"), prefix: prefix})
	}
	return sources
}

func (source downloadSource) fetch(address string, timeout time.Duration, consume func(io.Reader) error) error {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 6 * time.Second}).DialContext,
		TLSHandshakeTimeout:   6 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,
	}
	defer transport.CloseIdleConnections()
	if source.proxy != nil {
		transport.Proxy = http.ProxyURL(source.proxy)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 8 {
				return fmt.Errorf("无效的下载重定向")
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", source.prefix+address, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		// Signed asset URLs can carry query credentials; omit them from logs and the API.
		if failure, ok := err.(*url.Error); ok {
			err = failure.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}
	// An unresponsive or persistently slow source must not consume the full
	// package deadline while faster fallback sources remain.
	var progress chan int64
	if timeout > time.Minute {
		progress = make(chan int64, 1)
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			var total int64
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if total < 512000 {
						cancel()
						return
					}
					total = 0
				case count := <-progress:
					total += count
				}
			}
		}()
	}
	return consume(downloadReader{resp.Body, progress, ctx})
}

type downloadReader struct {
	io.Reader
	progress chan<- int64
	ctx      context.Context
}

func (r downloadReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.progress != nil {
		select {
		case r.progress <- int64(n):
		case <-r.ctx.Done():
		}
	}
	return n, err
}

func trySources(sources []downloadSource, log Logger, current func(string), attempt func(downloadSource) error) error {
	var last error
	for _, source := range sources {
		current(source.name)
		log.Logf("尝试 %s", source.name)
		if err := attempt(source); err != nil {
			last = err
			log.Logf("%s：%v", source.name, err)
			continue
		}
		return nil
	}
	return fmt.Errorf("所有下载来源均失败：%w", last)
}

func fetchUpdateRelease(address, expected string, sources []downloadSource, log Logger, current func(string)) (*updateRelease, error) {
	key, err := base64.RawStdEncoding.DecodeString(updatePublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("此安装包没有配置在线升级公钥")
	}
	var result *updateRelease
	err = trySources(sources, log, current, func(source downloadSource) error {
		return source.fetch(address, 15*time.Second, func(reader io.Reader) error {
			raw, err := io.ReadAll(io.LimitReader(reader, maxUpdateManifest+1))
			if err != nil {
				return err
			}
			if len(raw) > maxUpdateManifest {
				return fmt.Errorf("升级清单过大")
			}
			release, err := verifyRelease(raw, key)
			if err != nil {
				return err
			}
			if expected != "" && release.Version != expected {
				return fmt.Errorf("升级清单版本不匹配")
			}
			result = release
			return nil
		})
	})
	return result, err
}

func fetchUpdatePackage(address, target string, asset releaseAsset, sources []downloadSource, log Logger, current func(string)) error {
	return trySources(sources, log, current, func(source downloadSource) error {
		return source.fetch(address, 15*time.Minute, func(reader io.Reader) error {
			return storeUpdatePackage(target, asset, reader)
		})
	})
}

func storeUpdatePackage(target string, asset releaseAsset, reader io.Reader) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, asset.Size+1))
	closeErr := file.Close()
	if err == nil && (size != asset.Size || !strings.EqualFold(fmt.Sprintf("%x", hash.Sum(nil)), asset.SHA256)) {
		err = fmt.Errorf("升级包大小或 SHA-256 校验失败")
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(target)
	}
	return err
}

func installedProxySettings(p Paths, port int) (settings struct {
	MixedPort   int    `json:"mixed_port"`
	FakeIPRange string `json:"fakeip_range"`
}, err error) {
	api := newAPIClient(port, p.Data())
	api.http.Timeout = 4 * time.Second
	err = api.callOnce("GET", "/settings", nil, &settings)
	return
}
