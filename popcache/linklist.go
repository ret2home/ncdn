package main

type ListEntry struct {
	prev *ListEntry
	next *ListEntry
	val  string
}
type LinkList struct {
	head *ListEntry
	tail *ListEntry
	hand *ListEntry
}

func NewList() *LinkList {
	head := &ListEntry{
		prev: nil,
		next: nil,
		val:  "",
	}
	tail := &ListEntry{
		prev: head,
		next: nil,
		val:  "",
	}
	head.next = tail
	return &LinkList{
		head: head,
		tail: tail,
		hand: tail,
	}
}
func (l *LinkList) InsertFront(key string) {
	new_entry := &ListEntry{
		prev: l.head,
		next: l.head.next,
		val:  key,
	}
	l.head.next.prev = new_entry
	l.head.next = new_entry
	if l.hand == l.tail {
		l.hand = new_entry
	}
}
func (l *LinkList) Remove(ptr *ListEntry) {
	if l.hand == ptr {
		l.MoveHand()
	}
	ptr.prev.next = ptr.next
	ptr.next.prev = ptr.prev
}

// 少なくとも 1 つ要素がある場合
func (l *LinkList) MoveHand() {
	if l.hand == l.head.next {
		l.hand = l.tail.prev
	} else {
		l.hand = l.hand.prev
	}
}
