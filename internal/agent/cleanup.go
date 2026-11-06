package agent

import (
	"context"
	"log"
	"time"

	"github.com/pixalabteam/deploy-agent/internal/dockerctl"
)

// CleanupImages frees disk after a successful deploy. It is best-effort:
// every failure is logged as a warning and never reported as a deploy failure,
// because the new release is already serving traffic by the time this runs.
//
// Only images of the deployed repository are removed, newest ImageKeepCount
// are kept (current + previous, so rollback needs no re-pull) along with any
// image recorded in the state of any project. Volumes are never touched.
func (d *Deployer) CleanupImages(t Target, deployedImage string) {
	// Fresh context: the deploy context may be nearly expired by now.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repository := dockerctl.RepositoryOf(deployedImage)
	refs, err := d.docker.ListImageRefs(ctx, repository)
	if err != nil {
		log.Printf("warning: cleanup: list images of %s: %v", repository, err)
	}

	protected := d.state.Images()
	for i, ref := range refs {
		if i < t.Project.ImageKeepCount || protected[ref] {
			continue
		}
		if err := d.docker.RemoveImage(ctx, ref); err != nil {
			log.Printf("warning: cleanup: remove image %s: %v", ref, err)
			continue
		}
		log.Printf("cleanup: removed old image %s", ref)
	}

	if err := d.docker.PruneDanglingImages(ctx); err != nil {
		log.Printf("warning: cleanup: prune dangling images: %v", err)
	}
	if err := d.docker.PruneBuildCache(ctx, d.cfg.BuildCacheMaxAge); err != nil {
		log.Printf("warning: cleanup: prune build cache: %v", err)
	}
}
