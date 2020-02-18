/*
Copyright 2014 The Kubernetes Authors.

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

package cache

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Indexer is a storage interface that lets you list objects using multiple indexing functions
// indexer是一个接口集合，在store的基础上扩展了索引能力。方便的让用户list 各种objects
// indexer的一种实现 client-go/tools/cache/store.go
type Indexer interface {
	Store
	// Retrieve list of objects that match on the named indexing function
	Index(indexName string, obj interface{}) ([]interface{}, error)
	// IndexKeys returns the set of keys that match on the named indexing function.
	IndexKeys(indexName, indexKey string) ([]string, error)
	// ListIndexFuncValues returns the list of generated values of an Index func
	ListIndexFuncValues(indexName string) []string
	// ByIndex lists object that match on the named indexing function with the exact key
	ByIndex(indexName, indexKey string) ([]interface{}, error)
	// GetIndexer return the indexers
	GetIndexers() Indexers

	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	AddIndexers(newIndexers Indexers) error
}

// IndexFunc knows how to provide an indexed value for an object.
type IndexFunc func(obj interface{}) ([]string, error)

// IndexFuncToKeyFuncAdapter adapts an indexFunc to a keyFunc.  This is only useful if your index function returns
// unique values for every object.  This is conversion can create errors when more than one key is found.  You
// should prefer to make proper key and index functions.
func IndexFuncToKeyFuncAdapter(indexFunc IndexFunc) KeyFunc {
	return func(obj interface{}) (string, error) {
		indexKeys, err := indexFunc(obj)
		if err != nil {
			return "", err
		}
		if len(indexKeys) > 1 {
			return "", fmt.Errorf("too many keys: %v", indexKeys)
		}
		if len(indexKeys) == 0 {
			return "", fmt.Errorf("unexpected empty indexKeys")
		}
		return indexKeys[0], nil
	}
}

const (
	NamespaceIndex string = "namespace"
)

// MetaNamespaceIndexFunc is a default index function that indexes based on an object's namespace
func MetaNamespaceIndexFunc(obj interface{}) ([]string, error) {
	meta, err := meta.Accessor(obj)
	if err != nil {
		return []string{""}, fmt.Errorf("object has no meta: %v", err)
	}
	return []string{meta.GetNamespace()}, nil
}

//需要区分索引key，对象key
//例如 indexName = "byUser", indexKey = "wooniu", objKey = "pod1"

// Index maps the indexed value to a set of keys in the store that match on that value
// 找Pod有很多维度， 比如某一个namespace的所有podname，同一个节点的所有podname， 同一个service的所有podname, Index的key表示的一个维度。
// 例如： Index = {"ivanka":[]string{"pod1","pod2"}, "wits":[]string{"pod3","pod4"}}
// "ivanka", "wits" 都是按照NsIndexFunc(obj) 这个IndexFunc生成的
type Index map[string]sets.String

// Indexers maps a name to a IndexFunc
// 例如:
// Indexers := {
//	 "namespace": NsIndexFunc,
//	 "serviceName": SvcIndexFunc,
//	 "node": NodeIndexFunc,
// }
type Indexers map[string]IndexFunc

/* Indices maps a name to an Index
例如
Indices := {
	 "namespace": {
		 "ivanka":{"ivanka/pod1","ivanka/pod2"},
		 "wits":{"wits/pod3","wits/pod4"}
	 },
	 "svc": {
	 	 "ivankaqrain": {"ivanka/pod5","ivanka/pod6"},
	 	 "ivankacontent": {"ivnaka/pod7", "ivanka/pod8"}
	 },
	 "node": {
	 	 "node1": {"default/pod9","ivanka/pod10"},
	 	 "node2": {"ivanka/pod11", "wits/pod12"}
	 }
}

Indexers := {
	"namespace": GetNsFromObjFunc,
	"svc": GetSvcFromObjFunc,
	"node": GetNodeFromObjFunc
}

*/
type Indices map[string]Index
