package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	ssh2 "golang.org/x/crypto/ssh"
	"io"
	"log"
	"os"
	"path/filepath"
)

func main() {
	ctx := context.Background()

	rsc, err := readRepositoriesSyncConfiguration("config.json")
	if err != nil {
		log.Fatal(err)
	}

	for _, e := range rsc.SyncOptions {
		err = doRepositoriesSync(ctx, rsc, e)
		if err != nil {
			log.Fatal(err)
		}
	}
}

type RepositoryAccess struct {
	RepoName                  string `json:"repo_name"`
	RepoUrl                   string `json:"repo_url"`
	RepoPemFileName           string `json:"repo_pem_file_name"`
	RepoPemFilePassword       string `json:"repo_pem_file_password"`
	RepoSkipHostKeyValidation bool   `json:"repo_skip_host_key_validation"`
}

type RepositorySyncOption struct {
	SourceName      string `json:"source_name"`
	DestinationName string `json:"destination_name"`
}

type RepositoriesSyncConfiguration struct {
	ShadowsLocationBasePath string                  `json:"shadows_location_base_path"`
	Repositories            []*RepositoryAccess     `json:"repositories"`
	SyncOptions             []*RepositorySyncOption `json:"sync_options"`
}

func (rsc *RepositoriesSyncConfiguration) getRepositoryAccess(repoName string) *RepositoryAccess {
	for _, e := range rsc.Repositories {
		if e.RepoName == repoName {
			return e
		}
	}
	return nil
}

func readRepositoriesSyncConfiguration(configFileName string) (*RepositoriesSyncConfiguration, error) {
	configFile, err := os.Open(configFileName)
	if err != nil {
		return nil, err
	}
	defer func(configFile *os.File) {
		_ = configFile.Close()
	}(configFile)

	configBytes, err := io.ReadAll(configFile)
	if err != nil {
		return nil, err
	}

	var rsc RepositoriesSyncConfiguration
	err = json.Unmarshal(configBytes, &rsc)
	if err != nil {
		return nil, err
	}

	return &rsc, nil
}

func repositoryShadowCreateDir(sourceRepoUrl string, tmpPath string) (string, error) {
	hasher := sha256.New()
	_, err := hasher.Write([]byte(sourceRepoUrl))
	if err != nil {
		return "", err
	}
	sourceSha256sum := hex.EncodeToString(hasher.Sum(nil))
	sourceClonePath := filepath.Join(tmpPath, sourceSha256sum)
	err = os.MkdirAll(sourceClonePath, 644)
	if err != nil {
		return "", err
	}

	return sourceClonePath, nil
}

func repositorySshKeyRead(sourceRepoPemFileName string, sourceRepoPemFilePassword string, sourceRepoSkipHostKeyValidation bool) (*ssh.PublicKeys, error) {
	// Username must be "git" for SSH auth to work, not your real username.
	// See https://github.com/src-d/go-git/issues/637
	sourceRepoPublicKey, err := ssh.NewPublicKeysFromFile("git", sourceRepoPemFileName, sourceRepoPemFilePassword)
	if err != nil {
		return nil, err
	}
	if sourceRepoSkipHostKeyValidation {
		sourceRepoPublicKey.HostKeyCallback = ssh2.InsecureIgnoreHostKey()
	}
	return sourceRepoPublicKey, nil
}

func repositoryShadowCheckInit(sourceClonePath string) (*git.Repository, error) {
	r, err := git.PlainOpen(sourceClonePath)
	if err != nil && err != git.ErrRepositoryNotExists {
		return nil, err
	}
	if err != nil && err == git.ErrRepositoryNotExists {
		return nil, nil
	}
	return r, nil
}

func repositoryShadowInit(ctx context.Context, sourceClonePath string, sourceRepoUrl string, sourceRepoPublicKey *ssh.PublicKeys) (*git.Repository, error) {
	r, err := git.PlainInit(sourceClonePath, true)
	if err != nil {
		return nil, err
	}

	remoteSource, err := r.CreateRemote(&config.RemoteConfig{
		Name: git.DefaultRemoteName,
		URLs: []string{sourceRepoUrl},
		Fetch: []config.RefSpec{
			"+refs/*:refs/*",
		},
	})
	if err != nil {
		return nil, err
	}

	err = remoteSource.FetchContext(ctx, &git.FetchOptions{
		Auth:     sourceRepoPublicKey,
		Progress: os.Stdout,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return nil, err
	}

	return r, nil
}

func repositoryShadowUpdate(ctx context.Context, r *git.Repository, sourceClonePath string, sourceRepoPublicKey *ssh.PublicKeys) (*git.Repository, error) {
	r, err := git.PlainOpen(sourceClonePath)
	if err != nil {
		return nil, err
	}

	err = r.FetchContext(ctx, &git.FetchOptions{
		Auth:       sourceRepoPublicKey,
		RemoteName: git.DefaultRemoteName,
		Progress:   os.Stdout,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return nil, err
	}

	return r, nil
}

func repositoryShadowPushToNewOrigin(ctx context.Context, r *git.Repository, destinationRepoUrl string, destinationRepoPublicKey *ssh.PublicKeys) error {
	remoteDestination, err := r.Remote(git.DefaultRemoteName)
	if err != nil && err != git.ErrRemoteNotFound {
		return err
	}
	remoteDestination.Config().URLs = []string{
		destinationRepoUrl,
	}

	err = remoteDestination.PushContext(ctx, &git.PushOptions{
		Auth:     destinationRepoPublicKey,
		Progress: os.Stdout,
		RefSpecs: []config.RefSpec{
			"+refs/*:refs/*",
		},
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}

	return nil
}

func doRepositoriesSync(ctx context.Context, rsc *RepositoriesSyncConfiguration, syncOption *RepositorySyncOption) error {
	syncOptionSource := rsc.getRepositoryAccess(syncOption.SourceName)
	syncOptionDestination := rsc.getRepositoryAccess(syncOption.DestinationName)

	sourceClonePath, err := repositoryShadowCreateDir(syncOptionSource.RepoUrl, rsc.ShadowsLocationBasePath)
	if err != nil {
		return err
	}

	sourceRepoPublicKeys, err := repositorySshKeyRead(
		syncOptionSource.RepoPemFileName,
		syncOptionSource.RepoPemFilePassword,
		syncOptionSource.RepoSkipHostKeyValidation,
	)
	if err != nil {
		return err
	}

	r, err := repositoryShadowCheckInit(sourceClonePath)
	if err != nil {
		return err
	}

	if r == nil {
		r, err = repositoryShadowInit(ctx, sourceClonePath, syncOptionSource.RepoUrl, sourceRepoPublicKeys)
	} else {
		r, err = repositoryShadowUpdate(ctx, r, sourceClonePath, sourceRepoPublicKeys)
	}

	destinationRepoPublicKeys, err := repositorySshKeyRead(
		syncOptionDestination.RepoPemFileName,
		syncOptionDestination.RepoPemFilePassword,
		syncOptionDestination.RepoSkipHostKeyValidation,
	)
	if err != nil {
		return err
	}

	err = repositoryShadowPushToNewOrigin(ctx, r, syncOptionDestination.RepoUrl, destinationRepoPublicKeys)
	if err != nil {
		return err
	}

	return nil
}
