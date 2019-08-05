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

// Docker integration using pkg/kubelet/apis/cri/runtime/v1alpha2/api.pb.go
package dockershim

// dockershim为gGpc server， 实现了CRI接口，即实现了CRI要求实现的各个接口
// kubelet作为gGpc client调用dockershim的指定方法对container进行管理
