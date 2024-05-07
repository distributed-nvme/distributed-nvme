package restree

import (
	rbt "github.com/emirpasic/gods/v2/trees/redblacktree"
)

func qosCompare(x, y uint32) int {
	if x == 0 && y == 0 {
		return 0
	} else if x == 0 {
		return 1
	} else if y == 0 {
		return -1
	} else {
		switch {
		case x > y:
			return 1
		case x < y:
			return -1
		default:
			return 0
		}
	}
}

func cntCompare(x, y uint32) int {
	switch {
	case x > y:
		return 1
	case x < y:
		return -1
	default:
		return 0
	}
}

type Resource interface {
	GetQos() uint32
	GetCnt() uint32
	GetId() string
}

type ResourceTree struct {
	qosTree *rbt.Tree[uint32, *rbt.Tree[uint32, map[string]Resource]]
}

func (rt *ResourceTree) Put(res Resource) {
	var found bool
	var cntTree *rbt.Tree[uint32, map[string]Resource]
	var resMap map[string]Resource
	qos := res.GetQos()
	cnt := res.GetCnt()
	resId := res.GetId()
	cntTree, found = rt.qosTree.Get(qos)
	if !found {
		cntTree = rbt.NewWith[uint32, map[string]Resource](cntCompare)
		rt.qosTree.Put(qos, cntTree)
	}
	resMap, found = cntTree.Get(cnt)
	if !found {
		resMap = make(map[string]Resource)
		cntTree.Put(cnt, resMap)
	}
	_, ok := resMap[resId]
	if ok {
		panic("Put duplicate resId")
	}
	resMap[resId] = res
}

func (rt *ResourceTree) Remove(res Resource) {
	var found bool
	var cntTree *rbt.Tree[uint32, map[string]Resource]
	var resMap map[string]Resource
	qos := res.GetQos()
	cnt := res.GetCnt()
	resId := res.GetId()
	cntTree, found = rt.qosTree.Get(qos)
	if !found {
		panic("Remove unknow qos")
	}
	resMap, found = cntTree.Get(cnt)
	if !found {
		panic("Remove unknow cnt")
	}
	_, ok := resMap[resId]
	if !ok {
		panic("Remove unknow resId")
	}
	delete(resMap, resId)
	if len(resMap) == 0 {
		cntTree.Remove(cnt)
	}
	if cntTree.Size() == 0 {
		rt.qosTree.Remove(qos)
	}
}

func iterate(
	qosIterator *rbt.Iterator[uint32, *rbt.Tree[uint32, map[string]Resource]],
	apply func(res Resource) bool,
) {
	for qosIterator.Next() {
		cntTree := qosIterator.Value()
		cntIterator := cntTree.Iterator()
		for cntIterator.Next() {
			resMap := cntIterator.Value()
			for _, res := range resMap {
				ret := apply(res)
				if !ret {
					return
				}
			}
		}
	}
}

func (rt *ResourceTree) Iterate(apply func(res Resource) bool) {
	qosIterator := rt.qosTree.Iterator()
	iterate(qosIterator, apply)
}

func (rt *ResourceTree) IterateAt(qos uint32, apply func(res Resource) bool) {
	node, found := rt.qosTree.Ceiling(qos)
	if !found {
		return
	}
	cntIterator := node.Value.Iterator()
	for cntIterator.Next() {
		resMap := cntIterator.Value()
		for _, res := range resMap {
			ret := apply(res)
			if !ret {
				return
			}
		}
	}
	qosIterator := rt.qosTree.IteratorAt(node)
	iterate(qosIterator, apply)
}

func NewResourceTree() *ResourceTree {
	return &ResourceTree{
		qosTree: rbt.NewWith[uint32, *rbt.Tree[uint32, map[string]Resource]](qosCompare),
	}
}
