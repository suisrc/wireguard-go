package main

import (
	"bytes"
	"encoding/binary"
	"golang.org/x/crypto/chacha20poly1305"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type QueueHandshakeElement struct {
	msgType uint32
	packet  []byte
	source  *net.UDPAddr
}

type QueueInboundElement struct {
	dropped int32
	mutex   sync.Mutex
	packet  []byte
	counter uint64
	keyPair *KeyPair
}

func (elem *QueueInboundElement) Drop() {
	atomic.StoreInt32(&elem.dropped, AtomicTrue)
}

func (elem *QueueInboundElement) IsDropped() bool {
	return atomic.LoadInt32(&elem.dropped) == AtomicTrue
}

func addToInboundQueue(
	queue chan *QueueInboundElement,
	element *QueueInboundElement,
) {
	for {
		select {
		case queue <- element:
			return
		default:
			select {
			case old := <-queue:
				old.Drop()
			default:
			}
		}
	}
}

func addToHandshakeQueue(
	queue chan QueueHandshakeElement,
	element QueueHandshakeElement,
) {
	for {
		select {
		case queue <- element:
			return
		default:
			select {
			case <-queue:
			default:
			}
		}
	}
}

/* Routine determining the busy state of the interface
 *
 * TODO: prehaps nicer to do this in response to events
 * TODO: more well reasoned definition of "busy"
 */
func (device *Device) RoutineBusyMonitor() {
	samples := 0
	interval := time.Second
	for timer := time.NewTimer(interval); ; {

		select {
		case <-device.signal.stop:
			return
		case <-timer.C:
		}

		// compute busy heuristic

		if len(device.queue.handshake) > QueueHandshakeBusySize {
			samples += 1
		} else if samples > 0 {
			samples -= 1
		}
		samples %= 30
		busy := samples > 5

		// update busy state

		if busy {
			atomic.StoreInt32(&device.underLoad, AtomicTrue)
		} else {
			atomic.StoreInt32(&device.underLoad, AtomicFalse)
		}

		timer.Reset(interval)
	}
}

func (device *Device) RoutineReceiveIncomming() {

	logDebug := device.log.Debug
	logDebug.Println("Routine, receive incomming, started")

	var buffer []byte

	for {

		// check if stopped

		select {
		case <-device.signal.stop:
			return
		default:
		}

		// read next datagram

		if buffer == nil {
			buffer = make([]byte, MaxMessageSize)
		}

		device.net.mutex.RLock()
		conn := device.net.conn
		device.net.mutex.RUnlock()
		if conn == nil {
			time.Sleep(time.Second)
			continue
		}

		conn.SetReadDeadline(time.Now().Add(time.Second))

		size, raddr, err := conn.ReadFromUDP(buffer)
		if err != nil || size < MinMessageSize {
			continue
		}

		// handle packet

		packet := buffer[:size]
		msgType := binary.LittleEndian.Uint32(packet[:4])

		func() {
			switch msgType {

			case MessageInitiationType, MessageResponseType:

				// add to handshake queue

				addToHandshakeQueue(
					device.queue.handshake,
					QueueHandshakeElement{
						msgType: msgType,
						packet:  packet,
						source:  raddr,
					},
				)
				buffer = nil

			case MessageCookieReplyType:

				// verify and update peer cookie state

				if len(packet) != MessageCookieReplySize {
					return
				}

				var reply MessageCookieReply
				reader := bytes.NewReader(packet)
				err := binary.Read(reader, binary.LittleEndian, &reply)
				if err != nil {
					logDebug.Println("Failed to decode cookie reply")
					return
				}
				device.ConsumeMessageCookieReply(&reply)

			case MessageTransportType:

				// lookup key pair

				if len(packet) < MessageTransportSize {
					return
				}

				receiver := binary.LittleEndian.Uint32(
					packet[MessageTransportOffsetReceiver:MessageTransportOffsetCounter],
				)
				value := device.indices.Lookup(receiver)
				keyPair := value.keyPair
				if keyPair == nil {
					return
				}

				// check key-pair expiry

				if keyPair.created.Add(RejectAfterTime).Before(time.Now()) {
					return
				}

				// add to peer queue

				peer := value.peer
				work := new(QueueInboundElement)
				work.packet = packet
				work.keyPair = keyPair
				work.dropped = AtomicFalse
				work.mutex.Lock()

				// add to decryption queues

				addToInboundQueue(device.queue.decryption, work)
				addToInboundQueue(peer.queue.inbound, work)
				buffer = nil

			default:
				// unknown message type
				logDebug.Println("Got unknown message from:", raddr)
			}
		}()
	}
}

func (device *Device) RoutineDecryption() {
	var elem *QueueInboundElement
	var nonce [chacha20poly1305.NonceSize]byte

	logDebug := device.log.Debug
	logDebug.Println("Routine, decryption, started for device")

	for {
		select {
		case elem = <-device.queue.decryption:
		case <-device.signal.stop:
			return
		}

		// check if dropped

		if elem.IsDropped() {
			elem.mutex.Unlock()
			continue
		}

		// split message into fields

		counter := elem.packet[MessageTransportOffsetCounter:MessageTransportOffsetContent]
		content := elem.packet[MessageTransportOffsetContent:]

		// decrypt with key-pair

		var err error
		copy(nonce[4:], counter)
		elem.counter = binary.LittleEndian.Uint64(counter)
		elem.packet, err = elem.keyPair.receive.Open(elem.packet[:0], nonce[:], content, nil)
		if err != nil {
			elem.Drop()
		}
		elem.mutex.Unlock()
	}
}

/* Handles incomming packets related to handshake
 *
 *
 */
func (device *Device) RoutineHandshake() {

	logInfo := device.log.Info
	logError := device.log.Error
	logDebug := device.log.Debug
	logDebug.Println("Routine, handshake routine, started for device")

	var elem QueueHandshakeElement

	for {
		select {
		case elem = <-device.queue.handshake:
		case <-device.signal.stop:
			return
		}

		func() {

			// verify mac1

			if !device.mac.CheckMAC1(elem.packet) {
				logDebug.Println("Received packet with invalid mac1")
				return
			}

			// verify mac2

			busy := atomic.LoadInt32(&device.underLoad) == AtomicTrue

			if busy && !device.mac.CheckMAC2(elem.packet, elem.source) {
				sender := binary.LittleEndian.Uint32(elem.packet[4:8]) // "sender" always follows "type"
				reply, err := device.CreateMessageCookieReply(elem.packet, sender, elem.source)
				if err != nil {
					logError.Println("Failed to create cookie reply:", err)
					return
				}
				writer := bytes.NewBuffer(elem.packet[:0])
				binary.Write(writer, binary.LittleEndian, reply)
				elem.packet = writer.Bytes()
				_, err = device.net.conn.WriteToUDP(elem.packet, elem.source)
				if err != nil {
					logDebug.Println("Failed to send cookie reply:", err)
				}
				return
			}

			// ratelimit

			if !device.ratelimiter.Allow(elem.source.IP) {
				return
			}

			// handle messages

			switch elem.msgType {
			case MessageInitiationType:

				// unmarshal

				if len(elem.packet) != MessageInitiationSize {
					return
				}

				var msg MessageInitiation
				reader := bytes.NewReader(elem.packet)
				err := binary.Read(reader, binary.LittleEndian, &msg)
				if err != nil {
					logError.Println("Failed to decode initiation message")
					return
				}

				// consume initiation

				peer := device.ConsumeMessageInitiation(&msg)
				if peer == nil {
					logInfo.Println(
						"Recieved invalid initiation message from",
						elem.source.IP.String(),
						elem.source.Port,
					)
					return
				}

				// create response

				response, err := device.CreateMessageResponse(peer)
				if err != nil {
					logError.Println("Failed to create response message:", err)
					return
				}

				logDebug.Println("Creating response...")

				outElem := device.NewOutboundElement()
				writer := bytes.NewBuffer(outElem.data[:0])
				binary.Write(writer, binary.LittleEndian, response)
				outElem.packet = writer.Bytes()
				peer.mac.AddMacs(outElem.packet)
				addToOutboundQueue(peer.queue.outbound, outElem)

			case MessageResponseType:

				// unmarshal

				if len(elem.packet) != MessageResponseSize {
					return
				}

				var msg MessageResponse
				reader := bytes.NewReader(elem.packet)
				err := binary.Read(reader, binary.LittleEndian, &msg)
				if err != nil {
					logError.Println("Failed to decode response message")
					return
				}

				// consume response

				peer := device.ConsumeMessageResponse(&msg)
				if peer == nil {
					logInfo.Println(
						"Recieved invalid response message from",
						elem.source.IP.String(),
						elem.source.Port,
					)
					return
				}
				kp := peer.NewKeyPair()
				if kp == nil {
					logDebug.Println("Failed to derieve key-pair")
				}
				peer.SendKeepAlive()
				peer.EventHandshakeComplete()

			default:
				device.log.Error.Println("Invalid message type in handshake queue")
			}
		}()
	}
}

func (peer *Peer) RoutineSequentialReceiver() {
	var elem *QueueInboundElement

	device := peer.device
	logDebug := device.log.Debug
	logDebug.Println("Routine, sequential receiver, started for peer", peer.id)

	for {
		// wait for decryption

		select {
		case <-peer.signal.stop:
			return
		case elem = <-peer.queue.inbound:
		}
		elem.mutex.Lock()

		// process packet

		func() {
			if elem.IsDropped() {
				return
			}

			// check for replay

			if !elem.keyPair.replayFilter.ValidateCounter(elem.counter) {
				return
			}

			// time (passive) keep-alive

			peer.TimerStartKeepalive()

			// refresh key material (rekey)

			peer.KeepKeyFreshReceiving()

			// check if confirming handshake

			kp := &peer.keyPairs
			kp.mutex.Lock()
			if kp.next == elem.keyPair {
				peer.EventHandshakeComplete()
				kp.previous = kp.current
				kp.current = kp.next
				kp.next = nil
			}
			kp.mutex.Unlock()

			// check for keep-alive

			if len(elem.packet) == 0 {
				return
			}

			// verify source and strip padding

			switch elem.packet[0] >> 4 {
			case IPv4version:

				// strip padding

				if len(elem.packet) < IPv4headerSize {
					return
				}

				field := elem.packet[IPv4offsetTotalLength : IPv4offsetTotalLength+2]
				length := binary.BigEndian.Uint16(field)
				elem.packet = elem.packet[:length]

				// verify IPv4 source

				dst := elem.packet[IPv4offsetDst : IPv4offsetDst+net.IPv4len]
				if device.routingTable.LookupIPv4(dst) != peer {
					return
				}

			case IPv6version:

				// strip padding

				if len(elem.packet) < IPv6headerSize {
					return
				}

				field := elem.packet[IPv6offsetPayloadLength : IPv6offsetPayloadLength+2]
				length := binary.BigEndian.Uint16(field)
				length += IPv6headerSize
				elem.packet = elem.packet[:length]

				// verify IPv6 source

				dst := elem.packet[IPv6offsetDst : IPv6offsetDst+net.IPv6len]
				if device.routingTable.LookupIPv6(dst) != peer {
					return
				}

			default:
				logDebug.Println("Receieved packet with unknown IP version")
				return
			}

			atomic.AddUint64(&peer.rxBytes, uint64(len(elem.packet)))
			addToInboundQueue(device.queue.inbound, elem)
		}()
	}
}

func (device *Device) RoutineWriteToTUN(tun TUNDevice) {
	logError := device.log.Error
	logDebug := device.log.Debug
	logDebug.Println("Routine, sequential tun writer, started")

	for {
		select {
		case <-device.signal.stop:
			return
		case elem := <-device.queue.inbound:
			_, err := tun.Write(elem.packet)
			if err != nil {
				logError.Println("Failed to write packet to TUN device:", err)
			}
		}
	}
}
