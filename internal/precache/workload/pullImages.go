/*
 * Copyright 2023 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this inputFilePath except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package workload

import (
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	log "github.com/sirupsen/logrus"
)

// MaxRetries is the max number of retries for pulling an image before marking it as failed
const MaxRetries int = 5

// Podman auth-file related constants
const (
	EnvAuthFile     string = "PULL_SECRET_PATH"
	DefaultAuthFile string = "/var/lib/kubelet/config.json"
)

// executeCmd execute shell commands
func executeCmd(suppressCmdOutput bool, cmd string, args ...string) (err error) {

	logger := log.StandardLogger()
	logWriter := logger.Writer()

	log.Debugf("Executing %s with args %s", cmd, args)
	execCmd := exec.Command(cmd, args...)

	if suppressCmdOutput {
		execCmd.Stdout = nil
		execCmd.Stderr = nil
	} else {
		execCmd.Stdout = logWriter
		execCmd.Stderr = logWriter
	}

	if err = execCmd.Run(); err != nil {
		if !suppressCmdOutput {
			log.Error(err)
		}
	}

	_ = logWriter.Close()
	return err
}

// CheckPodman verifies that podman is running by checking the version of podman
func CheckPodman() bool {
	if err := executeCmd(false, "podman", []string{"version"}...); err != nil {
		return false
	}
	return true
}

// podmanImgExists reports the existence of the given image via podman CLI
func podmanImgExists(image string) bool {
	if err := executeCmd(true, "podman", []string{"image", "exists", image}...); err != nil {
		return false
	}
	return true
}

// podmanImgPull pulls the specified image via podman CLI
func podmanImgPull(image, authFile string) error {
	args := []string{"pull", image}
	if authFile != "" {
		args = append(args, []string{"--authfile", authFile}...)
	}
	return executeCmd(false, "podman", args...)
}

// pullImage attempts to pull an image via podman CLI
func pullImage(image, authFile string, progress *precache.Progress) error {

	var err error
	for i := 0; i < MaxRetries; i++ {
		err = podmanImgPull(image, authFile)
		if err == nil {
			log.Infof("Successfully pulled image: %s", image)
			break
		} else {
			log.Infof("Attempt %d/%d: Failed to pull %s: %v", i+1, MaxRetries, image, err)
		}
	}
	// update precache progress tracker
	progress.Update(err == nil, image)

	// persist progress to file
	progress.Persist(precache.StatusFile)

	return err
}

// getAuthFile returns the auth file for podman
func getAuthFile() (string, error) {
	// Configure Podman auth file
	authFile := os.Getenv(EnvAuthFile)
	if authFile == "" {
		authFile = DefaultAuthFile
	}

	// Check if authFile exists
	if _, err := os.Stat(authFile); os.IsNotExist(err) {
		log.Errorf("Missing auth file for podman")
		return "", err
	}
	log.Info("Auth file for podman found.")

	return authFile, nil
}

// PullImages pulls a list of images using podman
func PullImages(precacheSpec []string) (progress *precache.Progress, err error) {

	// Get auth file for Podman
	authFile, err := getAuthFile()
	if err != nil {
		return progress, err
	}

	// Initialize progress tracking
	progress = &precache.Progress{
		Total:   len(precacheSpec),
		Pulled:  0,
		Skipped: 0,
		Failed:  0,
	}

	var pullSpec = make([]string, 0, len(precacheSpec))
	// Sift through image list to determine which images exist, and which need to be pulled
	log.Infof("Checking the pre-cache spec file images to determine if they need to be pulled...")
	var skip bool
	for _, image := range precacheSpec {
		skip = podmanImgExists(image)
		if !skip {
			pullSpec = append(pullSpec, image)
		} else {
			log.Infof("%s exists, skipping it...", image)
			progress.Skipped++
		}
	}
	log.Infof("Check complete: %d images need to be pulled!", len(pullSpec))

	// Create wait group and pull images
	var wg sync.WaitGroup
	numThreads, err := strconv.Atoi(os.Getenv(precache.EnvMaxPullThreads))
	if err != nil {
		numThreads = precache.DefaultMaxConcurrentPulls
	}
	threads := make(chan struct{}, numThreads)
	log.Infof("Configured precaching job to concurrently pull %d images.", numThreads)

	// Start pulling images
	for _, image := range pullSpec {
		threads <- struct{}{}
		wg.Add(1)
		go func(image string) {
			defer func() {
				<-threads
				wg.Done()
			}()
			err := pullImage(image, authFile, progress)

			if err != nil {
				log.Errorf("Failed to pull image: %s, error: %v", image, err)
			}
		}(image)
	}

	log.Info("Waiting for precaching threads to finish...")
	// Wait for all threads to complete
	wg.Wait()
	log.Info("All the precaching threads have finished.")

	// Log final progress
	progress.Log()

	// Store final precache progress report to file
	progress.Persist(precache.StatusFile)

	return progress, nil
}
