package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mayooot/gpu-docker-api/internal/xerrors"
	"strings"
	"time"

	"github.com/commander-cli/cmd"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/ngaut/log"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/pkg/errors"
	"github.com/siddontang/go/sync2"

	"github.com/mayooot/gpu-docker-api/internal/docker"
	"github.com/mayooot/gpu-docker-api/internal/etcd"
	"github.com/mayooot/gpu-docker-api/internal/model"
)

var volumeVersionMap = cmap.New[sync2.AtomicInt64]()

type VolumeService struct{}

func (vs *VolumeService) CreateVolume(spec *model.VolumeCreate) (resp volume.Volume, err error) {
	ctx := context.Background()
	if vs.existVolume(spec.Name) {
		return resp, errors.Wrapf(xerrors.NewVolumeExistedError(), "volume %s", spec.Name)
	}

	var opt volume.CreateOptions
	if len(spec.Name) != 0 {
		opt.Name = spec.Name
	}
	if len(spec.Size) != 0 {
		opt.DriverOpts = map[string]string{"size": spec.Size}
	}

	opt.Driver = "local"
	resp, err = vs.createVolume(ctx, model.EtcdVolumeInfo{
		Opt: &opt,
	})
	if err != nil {
		return resp, errors.WithMessage(err, "service.createVolume failed")
	}
	return
}

func (vs *VolumeService) createVolume(ctx context.Context, info model.EtcdVolumeInfo) (resp volume.Volume, err error) {
	version, ok := volumeVersionMap.Get(info.Opt.Name)
	if !ok {
		volumeVersionMap.Set(info.Opt.Name, 0)
	} else {
		volumeVersionMap.Set(info.Opt.Name, sync2.AtomicInt64(version.Add(1)))
	}

	info.Opt.Name = fmt.Sprintf("%s-%d", info.Opt.Name, version)
	resp, err = docker.Cli.VolumeCreate(ctx, *info.Opt)
	if err != nil {
		return resp, errors.Wrapf(err, "docker.VolumeCreate failed, opt: %+v", info)
	}

	val := &model.EtcdVolumeInfo{
		Opt:     info.Opt,
		Version: version.Get(),
	}
	WorkQueue <- etcd.PutKeyValue{
		Key:      info.Opt.Name,
		Value:    val.Serialize(),
		Resource: etcd.VolumePrefix,
	}
	log.Infof("serivce.createVolume, volume created successfully, name: %s, spec: %+v", resp.Name, info)
	return
}

func (vs *VolumeService) DeleteVolume(name string, spec *model.VolumeDelete) error {
	ctx := context.Background()
	if err := docker.Cli.VolumeRemove(ctx, name, spec.Force); err != nil {
		return errors.Wrapf(err, "docker.VolumeRemove failed, name: %s", name)
	}

	if spec.DelEtcdInfo {
		WorkQueue <- etcd.DelKey{
			Resource: etcd.VolumePrefix,
			Key:      name,
		}
	}
	log.Infof("service.DeleteVolume, volume deleted successfully, name: %s", name)
	return nil
}

func (vs *VolumeService) PatchVolumeSize(name string, spec *model.VolumeSize) (resp volume.Volume, err error) {
	ctx := context.Background()
	infoBytes, err := etcd.Get(etcd.VolumePrefix, name)
	if err != nil {
		return resp, errors.WithMessage(err, "etcd.Get failed")
	}

	var info model.EtcdVolumeInfo
	if err = json.Unmarshal(infoBytes, &info); err != nil {
		return resp, errors.WithMessage(err, "json.Unmarshal failed")
	}

	// 更改 volume 的 size
	info.Opt.DriverOpts["size"] = spec.Size
	info.Opt.Name = strings.Split(name, "-")[0]
	resp, err = vs.createVolume(ctx, info)
	if err != nil {
		return resp, errors.WithMessage(err, "service.createVolume failed")
	}

	// 将旧的Volume 里的数据移到新的 Volume 中
	WorkQueue <- &copyTask{
		Resource:    etcd.VolumePrefix,
		OldResource: name,
		NewResource: resp.Name,
	}
	log.Infof("service.PatchVolumeSize, volume size patched successfully, name: %s, spec: %+v", name, spec)
	return
}

func (vs *VolumeService) volumeMountpoint(name string) (string, error) {
	ctx := context.Background()
	resp, err := docker.Cli.VolumeInspect(ctx, name)
	if err != nil || len(resp.Mountpoint) == 0 {
		return "", errors.Wrapf(err, "docker.VolumeInspect failed, name: %s", name)
	}

	return resp.Mountpoint, nil
}

func (vs *VolumeService) copyMountpointToContainer(task *copyTask) error {
	oldMountpoint, err := vs.volumeMountpoint(task.OldResource)
	if err != nil {
		return errors.WithMessage(err, "service.volumeMountpoint failed")
	}
	newMountpoint, err := vs.volumeMountpoint(task.NewResource)
	if err != nil {
		return errors.WithMessage(err, "service.volumeMountpoint failed")
	}

	if err = vs.copyMountpointFromOldVersion(oldMountpoint, newMountpoint); err != nil {
		return errors.WithMessage(err, "service.copyMountpointFromOldVersion failed")
	}

	return nil
}

func (vs *VolumeService) copyMountpointFromOldVersion(src, dest string) error {
	startT := time.Now()
	command := fmt.Sprintf(cpRFPOption, src, dest)
	if err := cmd.NewCommand(command).Execute(); err != nil {
		return errors.Wrapf(err, "cmd.Execute failed, command: %s, src:%s, dest: %s", command, src, dest)
	}
	log.Infof("service.copyMountpointFromOldVersion copy mountpoint successfully, src: %s, dest: %s, time cost: %v", src, dest, time.Since(startT))
	return nil
}

// 以 name- 为前缀的 volume 是否存在
func (vs *VolumeService) existVolume(name string) bool {
	ctx := context.Background()
	list, err := docker.Cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.KeyValuePair{Key: "name", Value: fmt.Sprintf("^%s-", name)}),
	})
	if err != nil || len(list.Volumes) == 0 {
		return false
	}

	return len(list.Volumes) > 0
}
