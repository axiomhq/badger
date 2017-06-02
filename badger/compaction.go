package badger

import (
	"bytes"
	"fmt"
	"log"
	"sync"

	"github.com/dgraph-io/badger/table"
	"github.com/dgraph-io/badger/y"
)

type keyRange struct {
	left  []byte
	right []byte
	inf   bool
}

var infRange = keyRange{inf: true}

func (r keyRange) String() string {
	return fmt.Sprintf("left=%q, right=%q, inf=%v", r.left, r.right, r.inf)
}

func (r keyRange) equals(dst keyRange) bool {
	return bytes.Compare(r.left, dst.left) == 0 &&
		bytes.Compare(r.right, dst.right) == 0 &&
		r.inf == dst.inf
}

func (r keyRange) overlapsWith(dst keyRange) bool {
	if r.inf || dst.inf {
		return true
	}

	// If my left is greater than dst right, we have no overlap.
	if bytes.Compare(r.left, dst.right) > 0 {
		return false
	}
	// If my right is less than dst left, we have no overlap.
	if bytes.Compare(r.right, dst.left) < 0 {
		return false
	}
	// We have overlap.
	return true
}

func getKeyRange(tables []*table.Table) keyRange {
	y.AssertTrue(len(tables) > 0)
	smallest := tables[0].Smallest()
	biggest := tables[0].Biggest()
	for i := 1; i < len(tables); i++ {
		if bytes.Compare(tables[i].Smallest(), smallest) < 0 {
			smallest = tables[i].Smallest()
		}
		if bytes.Compare(tables[i].Biggest(), biggest) > 0 {
			biggest = tables[i].Biggest()
		}
	}
	return keyRange{left: smallest, right: biggest}
}

type levelCompactStatus struct {
	ranges  []keyRange
	delSize int64
}

func (lcs levelCompactStatus) debug() string {
	var b bytes.Buffer
	for _, r := range lcs.ranges {
		b.WriteString(fmt.Sprintf("left=%q, right=%q inf=%v\n", r.left, r.right, r.inf))
	}
	return b.String()
}

func (lcs levelCompactStatus) overlapsWith(dst keyRange) bool {
	for _, r := range lcs.ranges {
		if r.overlapsWith(dst) {
			return true
		}
	}
	return false
}

func (lcs *levelCompactStatus) remove(dst keyRange) bool {
	final := lcs.ranges[:0]
	var found bool
	for _, r := range lcs.ranges {
		if !r.equals(dst) {
			final = append(final, r)
		} else {
			found = true
		}
	}
	lcs.ranges = final
	return found
}

type compactStatus struct {
	sync.RWMutex
	levels []*levelCompactStatus
}

func (cs *compactStatus) print() {
	cs.RLock()
	defer cs.RUnlock()

	fmt.Println("compaction status")
	for i, l := range cs.levels {
		fmt.Printf("[%d] %s\n", i, l.debug())
	}
}

func (cs *compactStatus) overlapsWith(level int, this keyRange) bool {
	cs.RLock()
	defer cs.RUnlock()

	thisLevel := cs.levels[level]
	return thisLevel.overlapsWith(this)
}

func (cs *compactStatus) delSize(l int) int64 {
	cs.RLock()
	defer cs.RUnlock()
	return cs.levels[l].delSize
}

func (cs *compactStatus) compareAndAdd(cd compactDef) bool {
	cs.Lock()
	defer cs.Unlock()

	level := cd.thisLevel.level

	y.AssertTruef(level < len(cs.levels)-1, "Got level %d. Max levels: %d", level, len(cs.levels))
	thisLevel := cs.levels[level]
	nextLevel := cs.levels[level+1]

	if thisLevel.overlapsWith(cd.thisRange) {
		return false
	}
	if nextLevel.overlapsWith(cd.nextRange) {
		return false
	}
	// Check whether this level really needs compaction or not. Otherwise, we'll end up
	// running parallel compactions for the same level.
	if cd.thisLevel.getTotalSize()-thisLevel.delSize < cd.thisLevel.maxTotalSize {
		return false
	}

	thisLevel.ranges = append(thisLevel.ranges, cd.thisRange)
	nextLevel.ranges = append(nextLevel.ranges, cd.nextRange)
	thisLevel.delSize += cd.thisSize

	fmt.Printf("======> compace and add. this level: %s next level: %s\n", thisLevel.debug(), nextLevel.debug())
	return true
}

func (cs *compactStatus) delete(cd compactDef) {
	cs.Lock()
	defer cs.Unlock()

	level := cd.thisLevel.level
	y.AssertTruef(level < len(cs.levels)-1, "Got level %d. Max levels: %d", level, len(cs.levels))

	thisLevel := cs.levels[level]
	nextLevel := cs.levels[level+1]

	thisLevel.delSize -= cd.thisSize
	found := thisLevel.remove(cd.thisRange)
	found = nextLevel.remove(cd.nextRange) && found

	if !found {
		this := cd.thisRange
		next := cd.nextRange
		fmt.Printf("Looking for: [%q, %q, %v] in this level.\n", this.left, this.right, this.inf)
		fmt.Printf("This Level:\n%s\n", thisLevel.debug())
		fmt.Println()
		fmt.Printf("Looking for: [%q, %q, %v] in next level.\n", next.left, next.right, next.inf)
		fmt.Printf("Next Level:\n%s\n", nextLevel.debug())
		log.Fatal("keyRange not found")
	}
}