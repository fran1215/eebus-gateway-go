package model

import (
	 "fmt"
)

type MessageQueue []Message

// Enqueue adds an element to the rear of the queue
func (q *MessageQueue) Enqueue(value Message) {
 *q = append(*q, value)
}

// Dequeue removes and returns an element from the front of the queue
func (q *MessageQueue) Dequeue() (Message, error) {
 if q.IsEmpty() {
  return Message{}, fmt.Errorf("empty queue")
 }
 value := (*q)[0]
 (*q)[0] = Message{} // Zero out the element (optional)
 *q = (*q)[1:]
 return value, nil
}

// CheckFront returns the front element without removing it
func (q *MessageQueue) CheckFront() (Message, error) {
 if q.IsEmpty() {
  return Message{}, fmt.Errorf("empty queue")
 }
 return (*q)[0], nil
}

// IsEmpty checks if the queue is empty
func (q *MessageQueue) IsEmpty() bool {
 return len(*q) == 0
}

// Size returns the number of elements in the queue
func (q *MessageQueue) Size() int {
 return len(*q)
}

// PrintQueue displays all elements in the queue
func (q *MessageQueue) PrintQueue() {
 if q.IsEmpty() {
  fmt.Println("Queue is empty")
  return
 }
 for _, item := range *q {
  fmt.Printf("%v ", item)
 }
 fmt.Println()
}