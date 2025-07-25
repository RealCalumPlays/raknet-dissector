package main

import (
	"context"
	"net"

	"github.com/Gskartwii/roblox-dissector/peer"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/olebedev/emitter"
)

func SrcAndDestFromGoPacket(packet gopacket.Packet) (*net.UDPAddr, *net.UDPAddr) {
	var srcIP, dstIP net.IP
	if ipv4, ok := packet.Layer(layers.LayerTypeIPv4).(*layers.IPv4); ok {
		srcIP = ipv4.SrcIP
		dstIP = ipv4.DstIP
	} else if ipv6, ok := packet.Layer(layers.LayerTypeIPv6).(*layers.IPv6); ok {
		srcIP = ipv6.SrcIP
		dstIP = ipv6.DstIP
	}
	return &net.UDPAddr{
			IP:   srcIP,
			Port: int(packet.Layer(layers.LayerTypeUDP).(*layers.UDP).SrcPort),
			Zone: "udp",
		}, &net.UDPAddr{
			IP:   dstIP,
			Port: int(packet.Layer(layers.LayerTypeUDP).(*layers.UDP).DstPort),
			Zone: "udp",
		}
}

func NewLayers(source *net.UDPAddr, dest *net.UDPAddr, fromClient bool) *peer.PacketLayers {
	return &peer.PacketLayers{
		Root: peer.RootLayer{
			Source:      source,
			Destination: dest,
			FromClient:  fromClient,
			FromServer:  !fromClient,
		},
	}
}

type PacketProvider interface {
	Layers() *emitter.Emitter
	Errors() *emitter.Emitter
}

type Conversations interface {
	ConversationFor(source *net.UDPAddr, dest *net.UDPAddr, payload []byte) *Conversation
	SetProgress(int)
}

func CaptureFromSource(ctx context.Context, convs Conversations, packetSource *gopacket.PacketSource) error {
	var progress int
	packetChan := packetSource.Packets()
	for {
		select {
		case <-ctx.Done():
			print("done")
			return nil
		case packet, ok := <-packetChan:
			if !ok {
				return nil
			}
			progress++

			if packet.ApplicationLayer() == nil ||
				(packet.Layer(layers.LayerTypeIPv4) == nil && packet.Layer(layers.LayerTypeIPv6) == nil) ||
				packet.Layer(layers.LayerTypeUDP) == nil {
				continue
			}
			payload := packet.ApplicationLayer().Payload()
			if len(payload) == 0 {
				continue
			}

			src, dest := SrcAndDestFromGoPacket(packet)
			conv := convs.ConversationFor(src, dest, payload)
			if conv == nil {
				continue // Not a RakNet packet
			}
			fromClient := AddressEq(src, conv.Client)

			// Write to PCAP if this is a CaptureSession
			if session, ok := convs.(*CaptureSession); ok {
				session.WritePacketToPCAP(src, dest, payload)
			}

			layers := NewLayers(src, dest, fromClient)
			var reader PacketProvider
			if fromClient {
				reader = conv.ClientReader
			} else {
				reader = conv.ServerReader
			}
			reader.(peer.PacketReader).ReadPacket(payload, layers)
			convs.SetProgress(progress)
		}
	}
}

func CaptureFromHandle(ctx context.Context, convs Conversations, handle *pcap.Handle) error {
	err := handle.SetBPFFilter("udp")
	if err != nil {
		return err
	}

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	return CaptureFromSource(ctx, convs, packetSource)
}
