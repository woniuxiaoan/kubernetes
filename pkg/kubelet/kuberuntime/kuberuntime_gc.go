/*
Copyright 2016 The Kubernetes Authors.

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

package kuberuntime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	internalapi "k8s.io/kubernetes/pkg/kubelet/apis/cri"
	runtimeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// containerGC is the manager of garbage collection.
type containerGC struct {
	client           internalapi.RuntimeService
	manager          *kubeGenericRuntimeManager
	podStateProvider podStateProvider
}

// NewContainerGC creates a new containerGC.
func NewContainerGC(client internalapi.RuntimeService, podStateProvider podStateProvider, manager *kubeGenericRuntimeManager) *containerGC {
	return &containerGC{
		client:           client,
		manager:          manager,
		podStateProvider: podStateProvider,
	}
}

// containerGCInfo is the internal information kept for containers being considered for GC.
type containerGCInfo struct {
	// The ID of the container.
	id string
	// The name of the container.
	name string
	// Creation time for the container.
	createTime time.Time
}

// sandboxGCInfo is the internal information kept for sandboxes being considered for GC.
type sandboxGCInfo struct {
	// The ID of the sandbox.
	id string
	// Creation time for the sandbox.
	createTime time.Time
	// If true, the sandbox is ready or still has containers.
	active bool
}

// evictUnit is considered for eviction as units of (UID, container name) pair.
type evictUnit struct {
	// UID of the pod.
	uid types.UID
	// Name of the container in the pod.
	name string
}

type containersByEvictUnit map[evictUnit][]containerGCInfo
type sandboxesByPodUID map[types.UID][]sandboxGCInfo

// NumContainers returns the number of containers in this map.
func (cu containersByEvictUnit) NumContainers() int {
	num := 0
	for key := range cu {
		num += len(cu[key])
	}
	return num
}

// NumEvictUnits returns the number of pod in this map.
func (cu containersByEvictUnit) NumEvictUnits() int {
	return len(cu)
}

// Newest first.
type byCreated []containerGCInfo

func (a byCreated) Len() int           { return len(a) }
func (a byCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// Newest first.
type sandboxByCreated []sandboxGCInfo

func (a sandboxByCreated) Len() int           { return len(a) }
func (a sandboxByCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a sandboxByCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// enforceMaxContainersPerEvictUnit enforces MaxPerPodContainer for each evictUnit.
func (cgc *containerGC) enforceMaxContainersPerEvictUnit(evictUnits containersByEvictUnit, MaxContainers int) {
	for key := range evictUnits {
		toRemove := len(evictUnits[key]) - MaxContainers

		if toRemove > 0 {
			evictUnits[key] = cgc.removeOldestN(evictUnits[key], toRemove)
		}
	}
}

// removeOldestN removes the oldest toRemove containers and returns the resulting slice.
func (cgc *containerGC) removeOldestN(containers []containerGCInfo, toRemove int) []containerGCInfo {
	// Remove from oldest to newest (last to first).
	numToKeep := len(containers) - toRemove
	for i := len(containers) - 1; i >= numToKeep; i-- {
		if err := cgc.manager.removeContainer(containers[i].id); err != nil {
			glog.Errorf("Failed to remove container %q: %v", containers[i].id, err)
		}
	}

	// Assume we removed the containers so that we're not too aggressive.
	return containers[:numToKeep]
}

// removeOldestNSandboxes removes the oldest inactive toRemove sandboxes and
// returns the resulting slice.
func (cgc *containerGC) removeOldestNSandboxes(sandboxes []sandboxGCInfo, toRemove int) {
	// Remove from oldest to newest (last to first).
	numToKeep := len(sandboxes) - toRemove
	for i := len(sandboxes) - 1; i >= numToKeep; i-- {
		if !sandboxes[i].active {
			cgc.removeSandbox(sandboxes[i].id)
		}
	}
}

// removeSandbox removes the sandbox by sandboxID.
func (cgc *containerGC) removeSandbox(sandboxID string) {
	glog.V(4).Infof("Removing sandbox %q", sandboxID)
	// In normal cases, kubelet should've already called StopPodSandbox before
	// GC kicks in. To guard against the rare cases where this is not true, try
	// stopping the sandbox before removing it.
	if err := cgc.client.StopPodSandbox(sandboxID); err != nil {
		glog.Errorf("Failed to stop sandbox %q before removing: %v", sandboxID, err)
		return
	}
	if err := cgc.client.RemovePodSandbox(sandboxID); err != nil {
		glog.Errorf("Failed to remove sandbox %q: %v", sandboxID, err)
	}
}

// evictableContainers gets all containers that are evictable. Evictable containers are: not running
// and created more than MinAge ago.
// 找到该节点上状态 !=running且创建时间大于MinAge的容器
// 搜集MinAge之前创建的且 not running的容器
func (cgc *containerGC) evictableContainers(minAge time.Duration) (containersByEvictUnit, error) {
	containers, err := cgc.manager.getKubeletContainers(true)
	if err != nil {
		return containersByEvictUnit{}, err
	}

	evictUnits := make(containersByEvictUnit)
	newestGCTime := time.Now().Add(-minAge)
	for _, container := range containers {
		// Prune out running containers.
		if container.State == runtimeapi.ContainerState_CONTAINER_RUNNING {
			continue
		}

		createdAt := time.Unix(0, container.CreatedAt)
		if newestGCTime.Before(createdAt) {
			continue
		}

		labeledInfo := getContainerInfoFromLabels(container.Labels)
		containerInfo := containerGCInfo{
			id:         container.Id,
			name:       container.Metadata.Name,
			createTime: createdAt,
		}
		key := evictUnit{
			uid:  labeledInfo.PodUID,
			name: containerInfo.name,
		}
		evictUnits[key] = append(evictUnits[key], containerInfo)
	}

	// Sort the containers by age.
	for uid := range evictUnits {
		sort.Sort(byCreated(evictUnits[uid]))
	}

	return evictUnits, nil
}

// evict all containers that are evictable
func (cgc *containerGC) evictContainers(gcPolicy kubecontainer.ContainerGCPolicy, allSourcesReady bool, evictTerminatedPods bool) error {
	// Separate containers by evict units.
	// 获取本轮GC可以进行操作的容器, 容器条件满足: not running && createAt >= MinAge ago
	// 有一个概念需要提前了解：
	//   1. 一个Pod可以包含多个Container
	//   2. 一个name的Container有些时候可能包含多个实例, 比如nginx container在频繁重启，那么在node节点上nginx名字对应的
	//      container就可能有多个
	evictUnits, err := cgc.evictableContainers(gcPolicy.MinAge)
	if err != nil {
		return err
	}

	// Remove deleted pod containers if all sources are ready.
	if allSourcesReady {
		for key, unit := range evictUnits {
			if cgc.podStateProvider.IsPodDeleted(key.uid) || (cgc.podStateProvider.IsPodTerminated(key.uid) && evictTerminatedPods) {
				cgc.removeOldestN(unit, len(unit)) // Remove all.
				delete(evictUnits, key)
			}
		}
	}

	// Enforce max containers per evict unit.
	// 对每个要操作的evitUint container数量进行整理，删除到里面的container只剩MaxPerPodContainer
	// 删除操作是调用containerd(http server)的指定接口进行的。
	// 删除每个evictableContainer中多过MaxPerPodContainer的容器，例如容器nginx在GC时有5个dead容器，而MaxPerPodContainer=1，则删除最早创建的那4个容器。
	if gcPolicy.MaxPerPodContainer >= 0 {
		cgc.enforceMaxContainersPerEvictUnit(evictUnits, gcPolicy.MaxPerPodContainer)
	}

	// Enforce max total number of containers.
	if gcPolicy.MaxContainers >= 0 && evictUnits.NumContainers() > gcPolicy.MaxContainers {
		// Leave an equal number of containers per evict unit (min: 1).
		// 根据MaxContainer数量，算出每个EvictUnit所能持有的not running状态的容器书数量， 超出的evictable container删除
		numContainersPerEvictUnit := gcPolicy.MaxContainers / evictUnits.NumEvictUnits()
		if numContainersPerEvictUnit < 1 {
			numContainersPerEvictUnit = 1
		}
		cgc.enforceMaxContainersPerEvictUnit(evictUnits, numContainersPerEvictUnit)

		// If we still need to evict, evict oldest first.
		// 如果总evictable 以旧 > MaxContainers, 则排序剩下所有的evictable container，按照时创建时间， 删除最早创建的(numContainers-MaxContainers)个容器
		numContainers := evictUnits.NumContainers()
		if numContainers > gcPolicy.MaxContainers {
			flattened := make([]containerGCInfo, 0, numContainers)
			for key := range evictUnits {
				flattened = append(flattened, evictUnits[key]...)
			}
			sort.Sort(byCreated(flattened))

			cgc.removeOldestN(flattened, numContainers-gcPolicy.MaxContainers)
		}
	}
	return nil
}

// evictSandboxes remove all evictable sandboxes. An evictable sandbox must
// meet the following requirements:
//   1. not in ready state
//   2. contains no containers.
//   3. belong to a non-existent (i.e., already removed) pod, or is not the
//      most recently created sandbox for the pod.
// 从上面可以看到这里Sandbox和Container是不同的，Container指的是真正的加入Sandbox namespace的容器
func (cgc *containerGC) evictSandboxes(evictTerminatedPods bool) error {
	containers, err := cgc.manager.getKubeletContainers(true)
	if err != nil {
		return err
	}

	// collect all the PodSandboxId of container
	// 拿到现有的所有container所对应的sandbox列表
	sandboxIDs := sets.NewString()
	for _, container := range containers {
		sandboxIDs.Insert(container.PodSandboxId)
	}

	sandboxes, err := cgc.manager.getKubeletSandboxes(true)
	if err != nil {
		return err
	}

	sandboxesByPod := make(sandboxesByPodUID)
	for _, sandbox := range sandboxes {
		// 通过annotation，container被k8分为了两种类型, container 和 sandbox
		// 每个container都有标识自己所属Pod信息的annotation, 包括(podName,podNamespace, podUID)
		// container类型container中包含有自己sandbox的id
		podUID := types.UID(sandbox.Metadata.Uid)
		sandboxInfo := sandboxGCInfo{
			id:         sandbox.Id,
			createTime: time.Unix(0, sandbox.CreatedAt),
		}

		// Set ready sandboxes to be active.
		if sandbox.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			sandboxInfo.active = true
		}

		// Set sandboxes that still have containers to be active.
		// 说明该sandbox 还有container, 则该sandbox还是active的。注意此时并没有要求container为running状态
		if sandboxIDs.Has(sandbox.Id) {
			sandboxInfo.active = true
		}

		// 获取本机所有Sandbox的信息(id,创建时间，状态)
		sandboxesByPod[podUID] = append(sandboxesByPod[podUID], sandboxInfo)
	}

	// Sort the sandboxes by age.
	// 按照创建时间，排序本机所有的sandbox
	for uid := range sandboxesByPod {
		sort.Sort(sandboxByCreated(sandboxesByPod[uid]))
	}

	// 删除的sandbox都是 State != runtimeapi.PodSandboxState_SANDBOX_READY && 没有对应的container
	for podUID, sandboxes := range sandboxesByPod {
		//如果某个Pod状态满足如下条件，则删除该Pod中所有状态非active的sandboxs
		if cgc.podStateProvider.IsPodDeleted(podUID) || (cgc.podStateProvider.IsPodTerminated(podUID) && evictTerminatedPods) {
			// Remove all evictable sandboxes if the pod has been removed.
			// Note that the latest dead sandbox is also removed if there is
			// already an active one.
			cgc.removeOldestNSandboxes(sandboxes, len(sandboxes))
		} else {
			// Keep latest one if the pod still exists.
			// 删除该Pod中最早创建的len(sandboxes)-1个sandbox中所有状态非active的sandbox
			cgc.removeOldestNSandboxes(sandboxes, len(sandboxes)-1)
		}
	}
	return nil
}

// evictPodLogsDirectories evicts all evictable pod logs directories. Pod logs directories
// are evictable if there are no corresponding pods.
// 如果该Pod已经被删除,则删除/var/log/pods下对应文件夹下的所有文件
// pods下的目录名称都是pod的uid
// 遍历/var/log/pods目录, 根据目录名称找到对用pod的uid, 然后查找该pod的状态, 如果该Pod已经被删除, 则删除该Pod对应文件夹的所有文件
func (cgc *containerGC) evictPodLogsDirectories(allSourcesReady bool) error {
	osInterface := cgc.manager.osInterface
	if allSourcesReady {
		// Only remove pod logs directories when all sources are ready.
		dirs, err := osInterface.ReadDir(podLogsRootDirectory)
		if err != nil {
			return fmt.Errorf("failed to read podLogsRootDirectory %q: %v", podLogsRootDirectory, err)
		}
		for _, dir := range dirs {
			name := dir.Name()
			podUID := types.UID(name)
			if !cgc.podStateProvider.IsPodDeleted(podUID) {
				continue
			}
			// 通过文件夹名字拿到podUID,根据UID判断该Pod是否为deleted, 如果是则删除该文件夹下的所有文件
			// 滚动更新就是这个情况, 滚动更新会删除老Pod, 所以此时该Pod的所有log文件就都会被删除掉
			err := osInterface.RemoveAll(filepath.Join(podLogsRootDirectory, name))
			if err != nil {
				glog.Errorf("Failed to remove pod logs directory %q: %v", name, err)
			}
		}
	}

	// Remove dead container log symlinks.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	// /var/log/containers/*.log
	logSymlinks, _ := osInterface.Glob(filepath.Join(legacyContainerLogsDir, fmt.Sprintf("*.%s", legacyLogSuffix)))
	for _, logSymlink := range logSymlinks {
		if _, err := osInterface.Stat(logSymlink); os.IsNotExist(err) {
			err := osInterface.Remove(logSymlink)
			if err != nil {
				glog.Errorf("Failed to remove container log dead symlink %q: %v", logSymlink, err)
			}
		}
	}
	return nil
}

// GarbageCollect removes dead containers using the specified container gc policy.
// Note that gc policy is not applied to sandboxes. Sandboxes are only removed when they are
// not ready and containing no containers.
//
// GarbageCollect consists of the following steps:
// * gets evictable containers which are not active and created more than gcPolicy.MinAge ago.
// * removes oldest dead containers for each pod by enforcing gcPolicy.MaxPerPodContainer.
// * removes oldest dead containers by enforcing gcPolicy.MaxContainers.
// * gets evictable sandboxes which are not ready and contains no containers.
// * removes evictable sandboxes.
// kubelet针对log的GC的目标只是针对 /var/log/contaienrs/xx 和 /var/log/pods, 因为这两个是kubernetes系统的
// /opt/docker/containers下面的log文件是docker daemon自己的log文件, 和kubernetes无关, 所以kubelet GC不会对此文件件做操作
// 当kubelet调用docker daemon接口删除容器时, /opt/docker/containers对应的container log会被删除. 也就是将kubelet是简介通过docker 删除的该文件夹下log
// 而不是直接管理删除的.
// woooniuzhang Kubelet 垃圾回收
func (cgc *containerGC) GarbageCollect(gcPolicy kubecontainer.ContainerGCPolicy, allSourcesReady bool, evictTerminatedPods bool) error {
	// Remove evictable containers
	// 按照设定的标准, 清除Pod中每个container的超额容器, 注意这里的container不包含sandbox
	// 通过该操作也会清除该container的对应log, 包括:
	//   1. /var/log/pods/{podUID}/{containerName}/{restartCounter}.log, 注意该log是/opt/docker/containers/{containerID}/xxx.log的软链
	//   2. /var/log/containers/(podName + podNamespace + containerName + containerID).log, 注意该log是上面log的软链
	//   !!! /opt/docker/containers/{containerID}/xxx.log才是实打实的log, 但是此步骤并没有删除该log
	if err := cgc.evictContainers(gcPolicy, allSourcesReady, evictTerminatedPods); err != nil {
		return err
	}

	// Remove sandboxes with zero containers
	// 以podUID为单位, 如果对应的pod已经删除, 那么则删除属于该Pod的所有sandbox， 删除sandbox不会删除log， 只是单纯的删sandbox
	// 如果pod不是deleted， 则删除该pod中所有状态不是active的sandbox. (只要sandbox还有对应的container在, 无论此container是死是活)
	if err := cgc.evictSandboxes(evictTerminatedPods); err != nil {
		return err
	}

	// Remove pod sandbox log directory
	// 遍历/var/log/pods文件夹, 通过dirname来获取pod状态, 如果pod已经删除, 则删除该pod对应的文件夹, 即/var/log/pods/{podUID}
	return cgc.evictPodLogsDirectories(allSourcesReady)
}
