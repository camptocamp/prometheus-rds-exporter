package main

import (
	"context"
	"dagger/prometheus-rds-exporter/internal/dagger"
	"fmt"
	"strings"
	"time"
)

const (
	RdsExporterBinaryName string = "prometheus-rds-exporter"

	githubCliVersion string = "2.53.0"
	githubRepository string = "camptocamp/prometheus-rds-exporter"
)

type RdsExporter struct {
	// +private
	Version string
	// +private
	Tag string

	Source *dagger.GitRef
}

func New(
	tag string,
) *RdsExporter {
	source := dag.Git("https://github.com/camptocamp/rds_exporter.git").
		Tag(tag)

	version := strings.TrimPrefix(tag, "v")

	rdsExporter := &RdsExporter{
		Version: version,
		Tag:     tag,
		Source:  source,
	}

	return rdsExporter
}

func (rdsExporter *RdsExporter) Binary(
	ctx context.Context,
	// +optional
	platform dagger.Platform,
) (*dagger.File, error) {
	if platform == "" {
		defaultPlatform, err := dag.DefaultPlatform(ctx)

		if err != nil {
			return nil, fmt.Errorf("failed to get platform: %s", err)
		}

		platform = defaultPlatform
	}

	platformElements := strings.Split(string(platform), "/")

	os := platformElements[0]
	arch := platformElements[1]

	commit, err := rdsExporter.Source.Commit(ctx)

	if err != nil {
		return nil, fmt.Errorf("failed to get commit hash: %s", err)
	}

	binary := dag.Golang().
		RedhatContainer().
		WithEnvVariable("GOOS", os).
		WithEnvVariable("GOARCH", arch).
		WithMountedDirectory(".", rdsExporter.Source.Tree()).
		WithExec([]string{
			"go", "build", "-o", RdsExporterBinaryName, "-ldflags", "-s -w " +
				fmt.Sprintf("-X 'github.com/prometheus/common/version.Version=%s'", rdsExporter.Version) + " " +
				fmt.Sprintf("-X 'github.com/prometheus/common/version.Revision=%s'", commit) + " " +
				fmt.Sprintf("-X 'github.com/prometheus/common/version.BuildDate=%s'", time.Now().Format("2006-01-02 15:04:05 -07:00")),
		}).
		File(RdsExporterBinaryName)

	return binary, nil
}

func (rdsExporter *RdsExporter) Overlay(
	ctx context.Context,
	// +optional
	platform dagger.Platform,
	// +optional
	prefix string,
) (*dagger.Directory, error) {
	if prefix == "" {
		prefix = "/usr/local"
	}

	binary, err := rdsExporter.Binary(ctx, platform)

	if err != nil {
		return nil, fmt.Errorf("failed to get binary: %s", err)
	}

	overlay := dag.Directory().
		WithDirectory(prefix, dag.Directory().
			WithDirectory("bin", dag.Directory().
				WithFile(RdsExporterBinaryName, binary),
			),
		)

	return overlay, nil
}

func (rdsExporter *RdsExporter) Container(
	ctx context.Context,
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	overlay, err := rdsExporter.Overlay(ctx, platform, "")

	if err != nil {
		return nil, fmt.Errorf("failed to get overlay: %s", err)
	}

	container := dag.Redhat().Micro().Container(dagger.RedhatMicroContainerOpts{Platform: platform}).
		WithDirectory("/", overlay).
		WithEntrypoint([]string{RdsExporterBinaryName}).
		WithDefaultArgs([]string{"--config.file=/etc/rds_exporter/config.yml"}).
		WithExposedPort(9042)

	return container, nil
}

func (rdsExporter *RdsExporter) Archive(
	ctx context.Context,
	// +optional
	platform dagger.Platform,
) (*dagger.File, error) {
	binary, err := rdsExporter.Binary(ctx, platform)

	if err != nil {
		return nil, fmt.Errorf("failed to get binary: %s", err)
	}

	archiveName := RdsExporterBinaryName + ".tar.gz"

	archive := dag.Redhat().Container().
		WithMountedDirectory(".", rdsExporter.Source.Tree()).
		WithMountedFile(RdsExporterBinaryName, binary).
		WithExec([]string{"tar", "-czvf", archiveName, RdsExporterBinaryName, "LICENSE", "CHANGELOG.md", "README.md"}).
		File(archiveName)

	return archive, nil
}

func (rdsExporter *RdsExporter) Release(
	ctx context.Context,
	githubToken *dagger.Secret,
) error {
	oses := []string{
		"linux",
		"darwin",
	}

	arches := []string{
		"amd64",
		"arm64",
	}

	archives := dag.Directory()

	for _, os := range oses {
		for _, arch := range arches {
			platform := dagger.Platform(os + "/" + arch)

			archive, err := rdsExporter.Archive(ctx, platform)

			if err != nil {
				return fmt.Errorf("failed to get archive for platform %s: %s", platform, err)
			}

			archives = archives.WithFile(fmt.Sprintf("%s-%s-%s.tar.gz", RdsExporterBinaryName, os, arch), archive)
		}
	}

	containers := make([]*dagger.Container, 0, len(oses)*len(arches))

	for _, arch := range arches {
		platform := dagger.Platform("linux/" + arch)

		container, err := rdsExporter.Container(ctx, platform)

		if err != nil {
			return fmt.Errorf("failed to get container for platform %s: %s", platform, err)
		}

		containers = append(containers, container)
	}

	archiveNames, err := archives.Entries(ctx)

	if err != nil {
		return fmt.Errorf("failed to get archive names: %s", err)
	}

	checksums, err := dag.Redhat().Container().
		WithMountedDirectory(".", archives).
		WithExec(append([]string{"sha256sum"}, archiveNames...)).
		Stdout(ctx)

	if err != nil {
		return fmt.Errorf("failed to compute checksums: %s", err)
	}

	_, err = dag.Container().
		WithRegistryAuth("ghcr.io", "dagger", githubToken).
		Publish(ctx, fmt.Sprintf("ghcr.io/%s:%s", githubRepository, rdsExporter.Version), dagger.ContainerPublishOpts{
			PlatformVariants: containers,
		})

	if err != nil {
		return fmt.Errorf("failed to publish container image manifest list: %s", err)
	}

	checksumsName := "checksums.txt"
	assetNames := append(archiveNames, checksumsName)

	_, err = dag.Github(githubCliVersion).RedhatContainer().
		WithEnvVariable("GH_REPO", githubRepository).
		WithSecretVariable("GH_TOKEN", githubToken).
		WithMountedDirectory(".", archives).
		WithNewFile(checksumsName, checksums).
		WithExec([]string{"gh", "release", "create", "--title", rdsExporter.Tag, rdsExporter.Tag}).
		WithExec(append([]string{"gh", "release", "upload", rdsExporter.Tag}, assetNames...)).
		Sync(ctx)

	if err != nil {
		return fmt.Errorf("failed to create release: %s", err)
	}

	return nil
}
