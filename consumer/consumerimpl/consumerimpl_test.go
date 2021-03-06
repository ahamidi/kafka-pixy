package consumerimpl

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/config"
	"github.com/mailgun/kafka-pixy/consumer"
	"github.com/mailgun/kafka-pixy/consumer/partitioncsm"
	"github.com/mailgun/kafka-pixy/offsetmgr"
	"github.com/mailgun/kafka-pixy/testhelpers"
	"github.com/mailgun/kafka-pixy/testhelpers/kafkahelper"
	log "github.com/sirupsen/logrus"
	. "gopkg.in/check.v1"
)

func Test(t *testing.T) {
	TestingT(t)
}

type ConsumerSuite struct {
	ns  *actor.Descriptor
	cfg *config.Proxy
	kh  *kafkahelper.T
	omf offsetmgr.Factory
}

var _ = Suite(&ConsumerSuite{})

func (s *ConsumerSuite) SetUpSuite(c *C) {
	testhelpers.InitLogging()
	s.kh = kafkahelper.New(c)

	cfg := testhelpers.NewTestProxyCfg("omf")
	tid := actor.Root().NewChild("omf")
	s.omf = offsetmgr.SpawnFactory(tid, cfg, s.kh.KafkaClt())
}

func (s *ConsumerSuite) TearDownSuite(*C) {
	s.omf.Stop()
	s.kh.Close()
}

func (s *ConsumerSuite) SetUpTest(*C) {
	s.ns = actor.Root().NewChild("T")
	s.cfg = testhelpers.NewTestProxyCfg("c1")
	partitioncsm.FirstMessageFetchedCh = make(chan *partitioncsm.T, 100)
}

// If initial offset stored in Kafka is greater then the newest offset for a
// partition, then partition consumer will wait for the given offset to be
// reached by produced messages and the first message returned will the one
// with the initial offset.
func (s *ConsumerSuite) TestInitialOffsetTooLarge(c *C) {
	oldestOffsets := s.kh.GetOldestOffsets("test.1")
	newestOffsets := s.kh.GetNewestOffsets("test.1")
	log.Infof("*** test.1 offsets: oldest=%v, newest=%v", oldestOffsets, newestOffsets)

	om, err := s.omf.Spawn(s.ns, "g1", "test.1", 0)
	c.Assert(err, IsNil)
	om.SubmitOffset(offsetmgr.Offset{newestOffsets[0] + 3, ""})
	om.Stop()

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When
	_, err = cons.Consume("g1", "test.1")

	// Then
	c.Assert(err, Equals, consumer.ErrRequestTimeout)

	produced := s.kh.PutMessages("offset-too-large", "test.1", map[string]int{"key": 5})
	consumed := consume(c, cons, "g1", "test.1", 1, 5*time.Second)
	c.Assert(consumed["key"][0].Offset, Equals, newestOffsets[0]+3)
	assertMsg(c, consumed["key"][0], produced["key"][3])
}

// If a topic has only one partition then the consumer will retrieve messages
// in the order they were produced.
func (s *ConsumerSuite) TestSinglePartitionTopic(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.1")
	produced := s.kh.PutMessages("single", "test.1", map[string]int{"": 3})

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When/Then
	consumed := consume(c, cons, "g1", "test.1", 1, 5*time.Second)
	assertMsg(c, consumed[""][0], produced[""][0])
}

// If we stop one consumer and start another, the new one picks up where the
// previous one left off.
func (s *ConsumerSuite) TestSequentialConsume(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.1")
	produced := s.kh.PutMessages("sequencial", "test.1", map[string]int{"": 3})

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	log.Infof("*** GIVEN 1")
	consumed := consume(c, cons, "g1", "test.1", 2, 5*time.Second)
	assertMsg(c, consumed[""][0], produced[""][0])
	assertMsg(c, consumed[""][1], produced[""][1])

	// When: one consumer stopped and another one takes its place.
	log.Infof("*** WHEN")
	cons.Stop()
	cons, err = Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// Then: the second message is consumed.
	log.Infof("*** THEN")
	consumed = consume(c, cons, "g1", "test.1", 1, 5*time.Second, consumed)
	assertMsg(c, consumed[""][2], produced[""][2])
}

// If we consume from a topic that has several partitions then partitions are
// selected for consumption in random order.
func (s *ConsumerSuite) TestMultiplePartitions(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.4")
	s.kh.PutMessages("multiple.partitions", "test.4", map[string]int{"A": 100, "B": 100})

	log.Infof("*** GIVEN 1")
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When: exactly one half of all produced events is consumed.
	log.Infof("*** WHEN")
	consumed := consume(c, cons, "g1", "test.4", 1, 5*time.Second)
	// Wait until first messages from partitions `A` and `B` are fetched.
	waitFirstFetched(cons, 2)
	// Consume 100 messages total
	consumed = consume(c, cons, "g1", "test.4", 99, 20*time.Second, consumed)

	// Then: we have events consumed from both partitions more or less evenly.
	log.Infof("*** THEN")
	if len(consumed["A"]) == 0 || len(consumed["A"]) == 0 {
		c.Errorf("Not distributed: consumed[A]=%d, consumed[B]=%d", len(consumed["A"]), len(consumed["B"]))
	}
}

// Different topics can be consumed at the same time.
func (s *ConsumerSuite) TestMultipleTopics(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.1")
	s.kh.ResetOffsets("g1", "test.4")
	produced1 := s.kh.PutMessages("multiple.topics", "test.1", map[string]int{"A": 1})
	produced4 := s.kh.PutMessages("multiple.topics", "test.4", map[string]int{"B": 1, "C": 1})

	log.Infof("*** GIVEN 1")
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When
	log.Infof("*** WHEN")
	consumed := consume(c, cons, "g1", "test.4", 1, 5*time.Second)
	consumed = consume(c, cons, "g1", "test.1", 1, 5*time.Second, consumed)
	consumed = consume(c, cons, "g1", "test.4", 1, 5*time.Second, consumed)

	// Then
	log.Infof("*** THEN")
	assertMsg(c, consumed["A"][0], produced1["A"][0])
	assertMsg(c, consumed["B"][0], produced4["B"][0])
	assertMsg(c, consumed["C"][0], produced4["C"][0])
}

// If the same topic is consumed by different consumer groups, then consumption
// by one group does not affect the consumption by another.
func (s *ConsumerSuite) TestMultipleGroups(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.4")
	s.kh.ResetOffsets("g2", "test.4")
	s.kh.PutMessages("multi", "test.4", map[string]int{"A": 10, "B": 10, "C": 10})

	log.Infof("*** GIVEN 1")
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When
	log.Infof("*** WHEN")
	consumed1 := consume(c, cons, "g1", "test.4", 10, 5*time.Second)
	consumed2 := consume(c, cons, "g2", "test.4", 20, 5*time.Second)
	consumed1 = consume(c, cons, "g1", "test.4", 20, 5*time.Second, consumed1)
	consumed2 = consume(c, cons, "g2", "test.4", 10, 5*time.Second, consumed2)

	// Then: both groups consumed the same events
	log.Infof("*** THEN")
	c.Assert(consumed1, DeepEquals, consumed2)
}

// When there are more consumers in a group then partitions in a topic then
// some consumers get assigned no partitions and their consume requests timeout.
func (s *ConsumerSuite) TestTooFewPartitions(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.1")
	produced := s.kh.PutMessages("few", "test.1", map[string]int{"": 3})

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()
	log.Infof("*** GIVEN 1")
	// Consume first message to make `consumer-1` subscribe for `test.1`
	consumed := consume(c, cons, "g1", "test.1", 2, 5*time.Second)
	assertMsg(c, consumed[""][0], produced[""][0])

	// When:
	log.Infof("*** WHEN")
	cfg1 := testhelpers.NewTestProxyCfg("c2")
	omf1 := offsetmgr.SpawnFactory(s.ns, cfg1, s.kh.KafkaClt())
	defer omf1.Stop()
	cons1, err := Spawn(s.ns, cfg1, omf1)
	c.Assert(err, IsNil)
	defer cons1.Stop()
	_, err = cons1.Consume("g1", "test.1")

	// Then: `consumer-2` request times out, when `consumer-1` requests keep
	// return messages.
	log.Infof("*** THEN")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
	consume(c, cons, "g1", "test.1", 1, 5*time.Second, consumed)
	assertMsg(c, consumed[""][1], produced[""][1])
}

// When a new consumer joins a group the partitions get evenly redistributed
// among all consumers.
func (s *ConsumerSuite) TestRebalanceOnJoin(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.4")
	s.kh.PutMessages("join", "test.4", map[string]int{"A": 10, "B": 10})

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// Consume the first message to make the consumer join the group and
	// subscribe to the topic.
	log.Infof("*** GIVEN 1")
	consumed1 := consume(c, cons, "g1", "test.4", 1, 5*time.Second)
	// Wait until first messages from partitions `A` and `B` are fetched.
	waitFirstFetched(cons, 2)

	// Consume 4 messages and make sure that there are messages from both
	// partitions among them.
	log.Infof("*** GIVEN 2")
	consumed1 = consume(c, cons, "g1", "test.4", 4, 5*time.Second, consumed1)
	c.Assert(len(consumed1["A"]), Not(Equals), 0)
	c.Assert(len(consumed1["B"]), Not(Equals), 0)
	consumedBeforeJoin := len(consumed1["B"])

	// When: another consumer joins the group rebalancing occurs.
	log.Infof("*** WHEN")
	cfg1 := testhelpers.NewTestProxyCfg("c2")
	omf1 := offsetmgr.SpawnFactory(s.ns, cfg1, s.kh.KafkaClt())
	defer omf1.Stop()
	cons1, err := Spawn(s.ns, cfg1, omf1)
	c.Assert(err, IsNil)
	defer cons1.Stop()

	// Then:
	log.Infof("*** THEN")
	consumed2 := consume(c, cons1, "g1", "test.4", consumeAll, 5*time.Second)
	consumed1 = consume(c, cons, "g1", "test.4", consumeAll, 5*time.Second, consumed1)
	// Partition "A" has been consumed by `consumer-1` only
	c.Assert(len(consumed1["A"]), Equals, 10)
	c.Assert(len(consumed2["A"]), Equals, 0)
	// Partition "B" has been consumed by both consumers, but ever since
	// `consumer-2` joined the group the first one have not got any new messages.
	c.Assert(len(consumed1["B"]), Equals, consumedBeforeJoin)
	c.Assert(len(consumed2["B"]), Not(Equals), 0)
	c.Assert(len(consumed1["B"])+len(consumed2["B"]), Equals, 10)
	// `consumer-2` started consumer from where `consumer-1` left off.
	c.Assert(consumed2["B"][0].Offset, Equals, consumed1["B"][len(consumed1["B"])-1].Offset+1)
}

// When a consumer leaves a group the partitions get evenly redistributed
// among the remaining consumers.
func (s *ConsumerSuite) TestRebalanceOnLeave(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.4")
	produced := s.kh.PutMessages("leave", "test.4", map[string]int{"A": 10, "B": 10, "C": 10})

	var err error
	consumers := make([]*t, 3)
	for i := 0; i < 3; i++ {
		cfg := testhelpers.NewTestProxyCfg(fmt.Sprintf("c%d", i))
		omf := offsetmgr.SpawnFactory(s.ns, cfg, s.kh.KafkaClt())
		defer omf.Stop()
		consumers[i], err = Spawn(s.ns, cfg, omf)
		c.Assert(err, IsNil)
	}
	defer consumers[0].Stop()
	defer consumers[1].Stop()

	log.Infof("*** GIVEN 1")
	// Consume the first message to make the consumer join the group and
	// subscribe to the topic.
	consumed := make([]map[string][]consumer.Message, 3)
	for i := 0; i < 3; i++ {
		consumed[i] = consume(c, consumers[i], "g1", "test.4", 1, 5*time.Second)
	}
	// consumer[0] can consume the first message from any partition and
	// consumer[1] can consume the first message from either `B` or `C`.
	log.Infof("*** GIVEN 2")
	if len(consumed[0]["A"]) == 1 {
		if len(consumed[1]["B"]) == 1 {
			assertMsg(c, consumed[2]["B"][0], produced["B"][1])
		} else { // if len(consumed[1]["C"]) == 1 {
			assertMsg(c, consumed[2]["B"][0], produced["B"][0])
		}
	} else if len(consumed[0]["B"]) == 1 {
		if len(consumed[1]["B"]) == 1 {
			assertMsg(c, consumed[2]["B"][0], produced["B"][2])
		} else { // if len(consumed[1]["C"]) == 1 {
			assertMsg(c, consumed[2]["B"][0], produced["B"][1])
		}
	} else { // if len(consumed[0]["C"]) == 1 {
		if len(consumed[1]["B"]) == 1 {
			assertMsg(c, consumed[2]["B"][0], produced["B"][1])
		} else { // if len(consumed[1]["C"]) == 1 {
			assertMsg(c, consumed[2]["B"][0], produced["B"][0])
		}
	}
	consume(c, consumers[2], "g1", "test.4", 4, 5*time.Second, consumed[2])
	c.Assert(len(consumed[2]["B"]), Equals, 5)
	lastConsumedFromBby2 := consumed[2]["B"][4]

	for _, consumer := range consumers {
		drainFirstFetched(consumer)
	}

	// When
	log.Infof("*** WHEN")
	consumers[2].Stop()
	// Wait for partition `C` reassign back to consumer[1]
	waitFirstFetched(consumers[1], 1)

	// Then: partition `B` is reassigned to `consumer[1]` and it picks up where
	// `consumer[2]` left off.
	log.Infof("*** THEN")
	consumedSoFar := make(map[string]int)
	for _, consumedByOne := range consumed {
		for key, consumedWithKey := range consumedByOne {
			consumedSoFar[key] = consumedSoFar[key] + len(consumedWithKey)
		}
	}
	yetToBeConsumedBy1 := (len(produced["B"]) + len(produced["C"])) - (consumedSoFar["B"] + consumedSoFar["C"])
	log.Infof("*** Consumed so far: %v", consumedSoFar)
	log.Infof("*** Yet to be consumed by cons[1]: %v", yetToBeConsumedBy1)

	consumedBy1 := consume(c, consumers[1], "g1", "test.4", yetToBeConsumedBy1, 5*time.Second)
	c.Assert(len(consumedBy1["B"]), Equals, len(produced["B"])-consumedSoFar["B"])
	c.Assert(consumedBy1["B"][0].Offset, Equals, lastConsumedFromBby2.Offset+1)
}

// When a consumer registration times out the partitions that used to be
// assigned to it are redistributed among active consumers.
func (s *ConsumerSuite) TestRebalanceOnTimeout(c *C) {
	var wg sync.WaitGroup
	consumed := make([]map[string][]consumer.Message, 2)

	s.kh.ResetOffsets("g1", "test.4")
	s.kh.PutMessages("timeout", "test.4", map[string]int{"A": 10, "B": 10})

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	cfg1 := testhelpers.NewTestProxyCfg("c2")
	cfg1.Consumer.SubscriptionTimeout = 500 * time.Millisecond
	omf1 := offsetmgr.SpawnFactory(s.ns, cfg1, s.kh.KafkaClt())
	defer omf1.Stop()
	sc1, err := Spawn(s.ns, cfg1, omf1)
	c.Assert(err, IsNil)
	defer sc1.Stop()

	// Consume the first message to make the consumers join the group and
	// subscribe to the topic.
	log.Infof("*** GIVEN 1")
	consumed[0] = consume(c, cons, "g1", "test.4", 1, 5*time.Second)
	consumed[1] = consume(c, sc1, "g1", "test.4", 1, 5*time.Second)

	if len(consumed[0]["B"]) == 0 {
		c.Assert(len(consumed[0]["A"]), Equals, 1)
	} else {
		c.Assert(len(consumed[0]["A"]), Equals, 0)
	}
	c.Assert(len(consumed[1]["A"]), Equals, 0)
	c.Assert(len(consumed[1]["B"]), Equals, 1)

	// Consume 4 more messages to make sure that each consumer pulls from a
	// partition assigned to it.
	log.Infof("*** GIVEN 2")
	actor.Spawn(s.ns.NewChild("consume[0]"), &wg, func() {
		consumed[0] = consume(c, cons, "g1", "test.4", 4, 5*time.Second, consumed[0])
	})
	actor.Spawn(s.ns.NewChild("consume[1]"), &wg, func() {
		consumed[1] = consume(c, sc1, "g1", "test.4", 4, 5*time.Second, consumed[1])
	})
	wg.Wait()

	if len(consumed[0]["B"]) == 1 {
		c.Assert(len(consumed[0]["A"]), Equals, 4)
	} else {
		c.Assert(len(consumed[0]["A"]), Equals, 5)
	}
	c.Assert(len(consumed[1]["A"]), Equals, 0)
	c.Assert(len(consumed[1]["B"]), Equals, 5)

	drainFirstFetched(cons)

	// When: `consumer-2` registration timeout elapses, the partitions get
	// rebalanced so that `consumer-1` becomes assigned to all of them...
	log.Infof("*** WHEN")
	// Wait for partition `B` reassigned back to sc1.
	waitFirstFetched(cons, 1)

	// ...and consumes the remaining messages from all partitions.
	log.Infof("*** THEN")
	consumed[0] = consume(c, cons, "g1", "test.4", 10, 5*time.Second, consumed[0])
	c.Assert(len(consumed[0]["A"]), Equals, 10)
	c.Assert(len(consumed[0]["B"]), Equals, 5)
	c.Assert(len(consumed[1]["A"]), Equals, 0)
	c.Assert(len(consumed[1]["B"]), Equals, 5)
}

// A `ErrTooManyRequests` error can be returned if internal buffers are filled
// with in-flight consume requests.
func (s *ConsumerSuite) TestTooManyRequestsError(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.1")
	s.kh.PutMessages("join", "test.1", map[string]int{"A": 30})

	s.cfg.Consumer.ChannelBufferSize = 1
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When
	var tooManyRequestsCount int32
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, err := cons.Consume("g1", "test.1")
				if err == consumer.ErrTooManyRequests {
					atomic.AddInt32(&tooManyRequestsCount, 1)
				}
			}
		}()
	}
	wg.Wait()

	// Then
	c.Assert(tooManyRequestsCount, Not(Equals), 0)
	log.Infof("*** ErrTooMannyRequests was returned %d times", tooManyRequestsCount)
}

// If an attempt is made to consume from a topic that does not exist then the
// request times out after `Config.Consumer.LongPollingTimeout`.
func (s *ConsumerSuite) TestInvalidTopic(c *C) {
	// Given
	s.cfg.Consumer.LongPollingTimeout = 1 * time.Second
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// When
	_, err = cons.Consume("g1", "no-such-topic")

	// Then
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
}

// A topic that has a lot of partitions can be consumed.
func (s *ConsumerSuite) TestLotsOfPartitions(c *C) {
	// Given
	s.kh.ResetOffsets("g1", "test.64")

	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	// Consume should stop by timeout and nothing should be consumed.
	msg, err := cons.Consume("g1", "test.64")
	c.Assert(err, Equals, consumer.ErrRequestTimeout, Commentf("Unexpected message consumed, %v", msg))
	s.kh.PutMessages("lots", "test.64", map[string]int{"A": 7, "B": 13, "C": 169})

	// When
	log.Infof("*** WHEN")
	consumed := consume(c, cons, "g1", "test.64", 189, 10*time.Second)

	// Then
	log.Infof("*** THEN")
	c.Assert(7, Equals, len(consumed["A"]))
	c.Assert(13, Equals, len(consumed["B"]))
	c.Assert(169, Equals, len(consumed["C"]))
}

// When a topic is consumed by a consumer group for the first time, its head
// offset is committed, to make sure that subsequently submitted messages are
// consumed.
func (s *ConsumerSuite) TestNewGroup(c *C) {
	// Given
	s.kh.PutMessages("rand", "test.1", map[string]int{"A1": 1})

	group := fmt.Sprintf("g%d", time.Now().Unix())
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)

	// The very first consumption of a group is terminated by timeout because
	// the default offset is the topic head.
	msg, err := cons.Consume(group, "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout, Commentf("Unexpected message consumed, %v", msg))

	// When: consumer is stopped, the concrete head offset is committed.
	cons.Stop()

	// Then: message produced after that will be consumed by the new consumer
	// instance from the same group.
	produced := s.kh.PutMessages("rand", "test.1", map[string]int{"A2": 1})
	cons, err = Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()
	msg, err = cons.Consume(group, "test.1")
	c.Assert(err, IsNil)
	assertMsg(c, msg, produced["A2"][0])
}

// If a consumer stops consuming one of the topics for more than the
// subscription timeout, then the topic partitions are rebalanced between
// active consumers, but the consumer keeps consuming messages from other
// topics.
func (s *ConsumerSuite) TestTopicTimeout(c *C) {
	s.kh.ResetOffsets("g1", "test.1")
	s.kh.PutMessages("topic-expire", "test.1", map[string]int{"A": 10})
	s.kh.ResetOffsets("g1", "test.4")
	s.kh.PutMessages("topic-expire", "test.4", map[string]int{"B": 10})

	s.cfg.Consumer.LongPollingTimeout = 3000 * time.Millisecond
	s.cfg.Consumer.SubscriptionTimeout = 10000 * time.Millisecond
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	cfg1 := testhelpers.NewTestProxyCfg("c2")
	cfg1.Consumer.LongPollingTimeout = 3000 * time.Millisecond
	cfg1.Consumer.SubscriptionTimeout = 10000 * time.Millisecond
	omf1 := offsetmgr.SpawnFactory(s.ns, cfg1, s.kh.KafkaClt())
	defer omf1.Stop()
	cons1, err := Spawn(s.ns, cfg1, omf1)
	c.Assert(err, IsNil)
	defer cons1.Stop()

	// Consume the first message to make the consumers join the group and
	// subscribe to the topics.
	log.Infof("*** GIVEN 1")
	start := time.Now()
	consumedTest1ByCons1 := consume(c, cons, "g1", "test.1", 1, 5*time.Second)
	c.Assert(len(consumedTest1ByCons1["A"]), Equals, 1)
	consumedTest4ByCons1 := consume(c, cons, "g1", "test.4", 1, 5*time.Second)
	c.Assert(len(consumedTest4ByCons1["B"]), Equals, 1)
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)

	delay := (5000 * time.Millisecond) - time.Now().Sub(start)
	log.Infof("*** sleeping for %v", delay)
	time.Sleep(delay)

	log.Infof("*** GIVEN 2:")
	consumedTest4ByCons1 = consume(c, cons, "g1", "test.4", 1, 5*time.Second, consumedTest4ByCons1)
	c.Assert(len(consumedTest4ByCons1["B"]), Equals, 2)
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)

	// When: wait for the cons1 subscription to test.1 topic to expire.
	log.Infof("*** WHEN")
	delay = (10100 * time.Millisecond) - time.Now().Sub(start)
	log.Infof("*** sleeping for %v", delay)
	time.Sleep(delay)

	// Then: the test.1 only partition is reassigned to cons2.
	log.Infof("*** THEN")
	consumedTest1ByCons2 := consume(c, cons1, "g1", "test.1", 1, 10*time.Second)
	c.Assert(len(consumedTest1ByCons2["A"]), Equals, 1)
	consumedTest4ByCons1 = consume(c, cons, "g1", "test.4", 1, 5*time.Second, consumedTest4ByCons1)
	c.Assert(len(consumedTest4ByCons1["B"]), Equals, 3)
}

// If there is an unacked message then topic subscription does not expire until
// the ack timeout expires.
func (s *ConsumerSuite) TestTopicAndAckTimeouts(c *C) {
	s.kh.ResetOffsets("g1", "test.1")
	produced := s.kh.PutMessages("with-offers", "test.1", map[string]int{"A": 10})

	s.cfg.Consumer.LongPollingTimeout = 1000 * time.Millisecond
	s.cfg.Consumer.SubscriptionTimeout = 2000 * time.Millisecond
	s.cfg.Consumer.AckTimeout = 5000 * time.Millisecond
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	cfg1 := testhelpers.NewTestProxyCfg("c2")
	cfg1.Consumer.LongPollingTimeout = 1000 * time.Millisecond
	cfg1.Consumer.SubscriptionTimeout = 5000 * time.Millisecond
	omf1 := offsetmgr.SpawnFactory(s.ns, cfg1, s.kh.KafkaClt())
	defer omf1.Stop()
	cons1, err := Spawn(s.ns, cfg1, omf1)
	c.Assert(err, IsNil)
	defer cons1.Stop()

	log.Infof("*** cons.0 consumes 1 message")
	msg0, err := cons.Consume("g1", "test.1")
	c.Assert(err, IsNil)
	assertMsg(c, msg0, produced["A"][0])

	log.Infof("*** cons.1 consume try #1")
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
	log.Infof("*** cons.1 consume try #2")
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
	log.Infof("*** cons.1 consume try #3")
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
	log.Infof("*** cons.1 consume try #4")
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
	log.Infof("*** cons.1 consume try #5")
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)

	log.Infof("*** Pause for cons.0 subscription to expire")
	time.Sleep(1000 * time.Millisecond)

	log.Infof("*** cons.1 consumes 1 message")
	msg1, err := cons1.Consume("g1", "test.1")
	c.Assert(err, IsNil)
	assertMsg(c, msg1, produced["A"][0])
}

// After subscription timeout when the last message is acked then the consumer
// immediately unsubscribes.
func (s *ConsumerSuite) TestTopicExpireOnAck(c *C) {
	s.kh.ResetOffsets("g1", "test.1")
	produced := s.kh.PutMessages("with-offers", "test.1", map[string]int{"A": 10})

	s.cfg.Consumer.LongPollingTimeout = 1000 * time.Millisecond
	s.cfg.Consumer.SubscriptionTimeout = 1500 * time.Millisecond
	s.cfg.Consumer.AckTimeout = 42000 * time.Millisecond
	cons, err := Spawn(s.ns, s.cfg, s.omf)
	c.Assert(err, IsNil)
	defer cons.Stop()

	cfg1 := testhelpers.NewTestProxyCfg("c2")
	cfg1.Consumer.LongPollingTimeout = 2000 * time.Millisecond
	omf1 := offsetmgr.SpawnFactory(s.ns, cfg1, s.kh.KafkaClt())
	defer omf1.Stop()
	cons1, err := Spawn(s.ns, cfg1, omf1)
	c.Assert(err, IsNil)
	defer cons1.Stop()

	msg0, err := cons.Consume("g1", "test.1")
	c.Assert(err, IsNil)
	assertMsg(c, msg0, produced["A"][0])

	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)
	_, err = cons1.Consume("g1", "test.1")
	c.Assert(err, Equals, consumer.ErrRequestTimeout)

	// When
	msg0.EventsCh <- consumer.Ack(msg0.Offset)

	// Then
	msg1, err := cons1.Consume("g1", "test.1")
	c.Assert(err, IsNil)
	assertMsg(c, msg1, produced["A"][1])
}

func assertMsg(c *C, consMsg consumer.Message, prodMsg *sarama.ProducerMessage) {
	c.Assert(sarama.StringEncoder(consMsg.Value), Equals, prodMsg.Value)
	c.Assert(consMsg.Offset, Equals, prodMsg.Offset)
}

func (s *ConsumerSuite) compareMsg(consMsg consumer.Message, prodMsg *sarama.ProducerMessage) bool {
	return sarama.StringEncoder(consMsg.Value) == prodMsg.Value.(sarama.Encoder) && consMsg.Offset == prodMsg.Offset
}

const consumeAll = -1

func consume(c *C, sc *t, group, topic string, count int,
	timeout time.Duration, extend ...map[string][]consumer.Message) map[string][]consumer.Message {

	var consumed map[string][]consumer.Message
	if len(extend) == 0 {
		consumed = make(map[string][]consumer.Message)
	} else {
		consumed = extend[0]
	}

	start := time.Now()
	for i := 0; i != count; {
		msg, err := sc.Consume(group, topic)
		if err == consumer.ErrRequestTimeout {
			if time.Now().Sub(start) < timeout {
				continue
			}
			if count == consumeAll {
				return consumed
			}
			c.Errorf("Not enough messages consumed: expected=%d, actual=%d", count, i)
			return consumed
		}
		i++
		c.Assert(err, IsNil)
		logConsumed(sc, msg)
		// Acknowledge consumed messages immediately
		msg.EventsCh <- consumer.Ack(msg.Offset)
		msg.EventsCh = nil
		consumed[string(msg.Key)] = append(consumed[string(msg.Key)], msg)
	}
	return consumed
}

func logConsumed(sc *t, msg consumer.Message) {
	log.Infof("*** consumed: by=%s, topic=%s, partition=%d, offset=%d, message=%s",
		sc.actDesc.String(), msg.Topic, msg.Partition, msg.Offset, msg.Value)
}

func drainFirstFetched(c *t) {
	for {
		select {
		case <-partitioncsm.FirstMessageFetchedCh:
		default:
			return
		}
	}
}

func waitFirstFetched(c *t, count int) {
	var partitions []int32
	for i := 0; i < count; i++ {
		pc := <-partitioncsm.FirstMessageFetchedCh
		partitions = append(partitions, pc.Partition())
	}
	log.Infof("*** first messages fetched: partitions=%v", partitions)
}
