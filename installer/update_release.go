package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	updateManifestName = "atom-update-manifest.json"
	releaseBase        = "https://github.com/jasonxtt/atom/releases/"
	maxUpdatePackage   = 512 << 20
	maxUpdateManifest  = 64 << 10
)

// The release builder embeds the public key in all four installer binaries.
var updatePublicKey string
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
var packagePlatforms = []string{"amd64", "arm64", "amd64-musl", "arm64-musl"}

type releaseAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type updateRelease struct {
	Version string                  `json:"version"`
	Assets  map[string]releaseAsset `json:"assets"`
}

type signedRelease struct {
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

func releasePackageName(version, platform string) string {
	return "atom-" + version + "-linux-" + platform + ".tar.gz"
}

func updatePlatform(arch, distro string) (string, error) {
	if err := checkArch(arch); err != "" {
		return "", fmt.Errorf("%s", err)
	}
	switch distro {
	case "alpine":
		return arch + "-musl", nil
	case "debian", "ubuntu":
		return arch, nil
	default:
		return "", fmt.Errorf("当前系统没有对应的 Atom 升级包")
	}
}

func verifyRelease(raw []byte, public ed25519.PublicKey) (*updateRelease, error) {
	var signed signedRelease
	if len(public) != ed25519.PublicKeySize || json.Unmarshal(raw, &signed) != nil ||
		!ed25519.Verify(public, signed.Payload, signed.Signature) {
		return nil, fmt.Errorf("升级清单签名无效")
	}
	var release updateRelease
	if err := json.Unmarshal(signed.Payload, &release); err != nil ||
		!releaseVersion.MatchString(release.Version) || len(release.Assets) != len(packagePlatforms) {
		return nil, fmt.Errorf("升级清单格式无效")
	}
	for _, platform := range packagePlatforms {
		asset := release.Assets[platform]
		hash, err := hex.DecodeString(asset.SHA256)
		if asset.Name != releasePackageName(release.Version, platform) || asset.Size <= 0 ||
			asset.Size > maxUpdatePackage || err != nil || len(hash) != 32 {
			return nil, fmt.Errorf("升级清单中的 %s 安装包无效", platform)
		}
	}
	return &release, nil
}

func readReleaseKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("升级签名密钥必须是 PKCS#8 PEM 格式")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("升级签名密钥必须是 Ed25519")
	}
	return private, nil
}

func signUpdateRelease(dir, version string, key ed25519.PrivateKey) error {
	if !releaseVersion.MatchString(version) {
		return fmt.Errorf("版本必须是 X.Y.Z")
	}
	release := updateRelease{Version: version, Assets: make(map[string]releaseAsset)}
	for _, platform := range packagePlatforms {
		name := releasePackageName(version, platform)
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		release.Assets[platform] = releaseAsset{name, info.Size(), fileSHA256(path)}
	}
	payload, err := json.Marshal(release)
	if err != nil {
		return err
	}
	envelope := signedRelease{payload, ed25519.Sign(key, payload)}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if _, err := verifyRelease(raw, key.Public().(ed25519.PublicKey)); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, updateManifestName), envelope)
}

func releaseSigningCommand(command string, args []string) error {
	if (command == "update-public-key" && len(args) != 1) ||
		(command == "sign-update" && len(args) != 3) {
		return fmt.Errorf("usage: update-public-key KEY | sign-update KEY DIST VERSION")
	}
	key, err := readReleaseKey(args[0])
	if err != nil {
		return err
	}
	if command == "update-public-key" {
		fmt.Println(base64.RawStdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		return nil
	}
	return signUpdateRelease(args[1], args[2], key)
}
