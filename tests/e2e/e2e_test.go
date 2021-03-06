/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/cloud"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/wait"
)

var (
	stdVolCap = []*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	stdVolSize  = int64(1 * 1024 * 1024 * 1024)
	stdCapRange = &csi.CapacityRange{RequiredBytes: stdVolSize}
)

var _ = Describe("EBS CSI Driver", func() {

	It("Should create, attach, stage and mount volume, check if it's writable, unmount, unstage, detach, delete, and check if it's deleted", func() {

		r1 := rand.New(rand.NewSource(time.Now().UnixNano()))
		req := &csi.CreateVolumeRequest{
			Name:               fmt.Sprintf("volume-name-e2e-test-%d", r1.Uint64()),
			CapacityRange:      stdCapRange,
			VolumeCapabilities: stdVolCap,
			Parameters:         nil,
		}

		resp, err := csiClient.ctrl.CreateVolume(context.Background(), req)
		Expect(err).To(BeNil(), "Could not create volume")

		volume := resp.GetVolume()
		Expect(volume).NotTo(BeNil(), "Expected valid volume, got nil")
		waitForVolumeState(volume.Id, "available")

		// Delete volume
		defer func() {
			_, err = csiClient.ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volume.Id})
			Expect(err).To(BeNil(), "Could not delete volume")
			waitForVolumes(volume.Id, 0 /* number of expected volumes */)

			// Deleting volume twice
			_, err = csiClient.ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volume.Id})
			Expect(err).To(BeNil(), "Error when trying to delete volume twice")
		}()

		// Attach, stage, publish, unpublish, unstage, detach
		nodeID := ebs.GetMetadata().GetInstanceID()
		testAttachWriteReadDetach(volume.Id, req.GetName(), nodeID, false)

	})
})

func testAttachWriteReadDetach(volumeID, volName, nodeID string, readOnly bool) {
	// Attach volume
	respAttach, err := csiClient.ctrl.ControllerPublishVolume(
		context.Background(),
		&csi.ControllerPublishVolumeRequest{
			VolumeId:         volumeID,
			NodeId:           nodeID,
			VolumeCapability: stdVolCap[0],
		},
	)
	Expect(err).To(BeNil(), "ControllerPublishVolume failed attaching volume %q to node %q", volumeID, nodeID)
	waitForVolumeState(volumeID, "in-use")

	// Detach Volume
	defer func() {
		_, err = csiClient.ctrl.ControllerUnpublishVolume(
			context.Background(),
			&csi.ControllerUnpublishVolumeRequest{
				VolumeId: volumeID,
				NodeId:   nodeID,
			},
		)
		Expect(err).To(BeNil(), "ControllerUnpublishVolume failed with error")
		waitForVolumeState(volumeID, "available")
	}()

	// Stage Disk
	volDir := filepath.Join("/tmp/", volName)
	stageDir := filepath.Join(volDir, "stage")
	_, err = csiClient.node.NodeStageVolume(
		context.Background(),
		&csi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stageDir,
			VolumeCapability:  stdVolCap[0],
			PublishInfo:       map[string]string{"devicePath": respAttach.PublishInfo["devicePath"]},
		})
	Expect(err).To(BeNil(), "NodeStageVolume failed with error")

	defer func() {
		// Unstage Disk
		_, err := csiClient.node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: volumeID, StagingTargetPath: stageDir})
		Expect(err).To(BeNil(), "NodeUnstageVolume failed with error")
		err = os.RemoveAll(volDir)
		Expect(err).To(BeNil(), "Failed to remove temp directory")
	}()

	// Mount Disk
	publishDir := filepath.Join("/tmp/", volName, "mount")
	_, err = csiClient.node.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stageDir,
		TargetPath:        publishDir,
		VolumeCapability:  stdVolCap[0],
	})
	Expect(err).To(BeNil(), "NodePublishVolume failed with error")

	// Unmount Disk
	defer func() {
		_, err = csiClient.node.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
			VolumeId:   volumeID,
			TargetPath: publishDir,
		})
		Expect(err).To(BeNil(), "NodeUnpublishVolume failed with error")
	}()

	if !readOnly {
		// Write a file
		testFileContents := []byte("sample content")
		testFile := filepath.Join(publishDir, "testfile")
		err := ioutil.WriteFile(testFile, testFileContents, 0644)
		Expect(err).To(BeNil(), "Failed to write file")
		// Read the file and check if content is correct
		data, err := ioutil.ReadFile(testFile)
		Expect(err).To(BeNil(), "Failed to read file")
		Expect(data).To(Equal(testFileContents), "File content is incorrect")
	}
}

func waitForVolumeState(volumeID, state string) {
	// Most attach/detach operations on AWS finish within 1-4 seconds.
	// By using 1 second starting interval with a backoff of 1.8,
	// we get [1, 1.8, 3.24, 5.832000000000001, 10.4976].
	// In total we wait for 2601 seconds.
	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1.8,
		Steps:    13,
	}
	verifyVolumeFunc := func() (bool, error) {
		params := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(volumeID)},
		}
		volumes, err := describeVolumes(params)
		if err != nil {
			return false, err
		}
		if len(volumes) != 1 {
			return false, fmt.Errorf("expected 1 volume, got %d", len(volumes))
		}
		if aws.StringValue(volumes[0].State) != state {
			return false, nil
		}
		// We need to check the atachment state when the volume is "in-use",
		// as it might still be "attaching" rather than "attached".
		if state == "in-use" {
			if aws.StringValue(volumes[0].Attachments[0].State) != "attached" {
				return false, nil
			}
		}
		return true, nil
	}
	waitErr := wait.ExponentialBackoff(backoff, verifyVolumeFunc)
	Expect(waitErr).To(BeNil(), "Timeout error waiting for volume state %q: %v", waitErr, state)
}

func waitForVolumes(volumeID string, nVolumes int) {
	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1.8,
		Steps:    13,
	}
	verifyVolumeFunc := func() (bool, error) {
		params := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(volumeID)},
		}
		volumes, err := describeVolumes(params)
		if err != nil {
			if nVolumes == 0 {
				if awsErr, ok := err.(awserr.Error); ok {
					if awsErr.Code() == "InvalidVolume.NotFound" {
						return true, nil
					}
				}
			}
			return false, err
		}
		if len(volumes) != nVolumes {
			return false, nil
		}
		return true, nil
	}
	waitErr := wait.ExponentialBackoff(backoff, verifyVolumeFunc)
	Expect(waitErr).To(BeNil(), "Timeout error when looking for volume %q: %v", volumeID, waitErr)
}

func describeVolumes(params *ec2.DescribeVolumesInput) ([]*ec2.Volume, error) {
	var volumes []*ec2.Volume
	var nextToken *string
	for {
		response, err := ec2Client.DescribeVolumes(params)
		if err != nil {
			return nil, err
		}
		for _, volume := range response.Volumes {
			volumes = append(volumes, volume)
		}
		nextToken = response.NextToken
		if aws.StringValue(nextToken) == "" {
			break
		}
		params.NextToken = nextToken
	}
	return volumes, nil
}
