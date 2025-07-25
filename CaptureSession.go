package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Gskartwii/roblox-dissector/peer"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"github.com/gotk3/gotk3/glib"
	"github.com/olebedev/emitter"
)

type Conversation struct {
	Client       *net.UDPAddr
	Server       *net.UDPAddr
	ClientReader PacketProvider
	ServerReader PacketProvider
	Context      *peer.CommunicationContext
}

type CaptureSession struct {
	Name                  string
	ViewerCounter         uint
	IsCapturing           bool
	Conversations         []*Conversation
	CancelFunc            context.CancelFunc
	InitialViewerOccupied bool
	ListViewers           []*PacketListViewer
	ListViewerCallback    func(*CaptureSession, *PacketListViewer, error)
	ProgressCallback      func(int)
	progress              int
	ForgetAcks            bool
	// PCAP writing fields
	pcapFile   *os.File
	pcapWriter *pcapgo.Writer
}

func AddressEq(a *net.UDPAddr, b *net.UDPAddr) bool {
	return a.Port == b.Port && a.IP.Equal(b.IP)
}

func NewCaptureSession(name string, cancelFunc context.CancelFunc, listViewerCallback func(*CaptureSession, *PacketListViewer, error)) (*CaptureSession, error) {
	initialViewer, err := NewPacketListViewer(fmt.Sprintf("%s#%d", name, 1), nil)
	if err != nil {
		return nil, err
	}
	session := &CaptureSession{
		Name:                  name,
		ViewerCounter:         2,
		IsCapturing:           true,
		Conversations:         nil,
		CancelFunc:            cancelFunc,
		InitialViewerOccupied: false,
		ListViewers:           []*PacketListViewer{initialViewer},
		ListViewerCallback:    listViewerCallback,
	}
	listViewerCallback(session, initialViewer, nil)

	return session, nil
}

func (session *CaptureSession) SetProgress(prog int) {
	session.progress = prog
}

func (session *CaptureSession) ConversationFor(source *net.UDPAddr, dest *net.UDPAddr, payload []byte) *Conversation {
	for _, conv := range session.Conversations {
		if AddressEq(source, conv.Client) && AddressEq(dest, conv.Server) {
			return conv
		}
		if AddressEq(source, conv.Server) && AddressEq(dest, conv.Client) {
			return conv
		}
	}

	if len(payload) < 1 || payload[0] != 0x7B {
		return nil
	}
	isHandshake := peer.IsOfflineMessage(payload)
	if !isHandshake {
		return nil
	}

	newContext := peer.NewCommunicationContext()
	clientR := peer.NewPacketReader()
	serverR := peer.NewPacketReader()
	clientR.SetContext(newContext)
	serverR.SetContext(newContext)
	clientR.SetIsClient(true)
	clientR.BindDataModelHandlers()
	serverR.BindDataModelHandlers()
	newConv := &Conversation{
		Client:       source,
		Server:       dest,
		ClientReader: clientR,
		ServerReader: serverR,
		Context:      newContext,
	}
	session.Conversations = append(session.Conversations, newConv)
	session.AddConversation(newConv)

	return newConv
}

func (session *CaptureSession) AddConversation(conv *Conversation) (*PacketListViewer, error) {
	var err error
	var viewer *PacketListViewer
	if !session.InitialViewerOccupied {
		session.InitialViewerOccupied = true
		viewer = session.ListViewers[0]
		viewer.Conversation = conv
	} else {
		title := fmt.Sprintf("%s#%d", session.Name, session.ViewerCounter)
		session.ViewerCounter++

		glib.IdleAdd(func() bool {
			viewer, err = NewPacketListViewer(title, conv)
			session.ListViewerCallback(session, viewer, err)
			return false
		})
	}
	handler := func(e *emitter.Event) {
		topic := e.OriginalTopic
		layers := e.Args[0].(*peer.PacketLayers)

		associatedProgress := session.progress
		_, err := glib.IdleAdd(func() bool {
			viewer.NotifyPacket(topic, layers, session.ForgetAcks)
			if session.ProgressCallback != nil {
				session.ProgressCallback(associatedProgress)
			}
			return false
		})
		if err != nil {
			println("idleadd failed:", err.Error())
		}
	}
	conv.ClientReader.Layers().On("*", handler, emitter.Void)
	conv.ClientReader.Errors().On("*", handler, emitter.Void)
	conv.ServerReader.Layers().On("*", handler, emitter.Void)
	conv.ServerReader.Errors().On("*", handler, emitter.Void)

	return viewer, err
}

func (session *CaptureSession) StopCapture() {
	if session.IsCapturing {
		session.IsCapturing = false
		session.CancelFunc()
		// Close PCAP file if open
		session.ClosePCAPWriter()
	}
}

func (session *CaptureSession) ReportDone() {
	glib.IdleAdd(func() bool {
		session.IsCapturing = false
		if session.ProgressCallback != nil {
			session.ProgressCallback(-1)
		}
		// Close PCAP file when done
		session.ClosePCAPWriter()
		return false
	})
}

// InitPCAPWriter initializes PCAP file writing for the capture session
func (session *CaptureSession) InitPCAPWriter(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create PCAP file: %v", err)
	}

	session.pcapFile = file
	session.pcapWriter = pcapgo.NewWriter(file)
	err = session.pcapWriter.WriteFileHeader(65536, layers.LinkTypeEthernet)
	if err != nil {
		session.pcapFile.Close()
		return fmt.Errorf("failed to write PCAP header: %v", err)
	}

	return nil
}

// ClosePCAPWriter closes the PCAP file writer
func (session *CaptureSession) ClosePCAPWriter() {
	if session.pcapFile != nil {
		session.pcapFile.Close()
		session.pcapFile = nil
		session.pcapWriter = nil
	}
}

// WritePacketToPCAP writes a raw UDP packet payload to the PCAP file
func (session *CaptureSession) WritePacketToPCAP(srcAddr, dstAddr *net.UDPAddr, payload []byte) {
	if session.pcapWriter != nil {
		err := session.WriteToPCAP(srcAddr, dstAddr, payload)
		if err != nil {
			println("PCAP write error:", err.Error())
		}
	}
}
func (session *CaptureSession) WriteToPCAP(srcAddr, dstAddr *net.UDPAddr, payload []byte) error {
	if session.pcapWriter == nil {
		return nil // PCAP writer not initialized
	}

	// Create packet layers
	ethLayer := &layers.Ethernet{
		SrcMAC:       net.HardwareAddr{0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		DstMAC:       net.HardwareAddr{0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
		EthernetType: layers.EthernetTypeIPv4,
	}

	var ipLayer gopacket.SerializableLayer
	if srcAddr.IP.To4() != nil {
		ipLayer = &layers.IPv4{
			Version:  4,
			TTL:      64,
			Protocol: layers.IPProtocolUDP,
			SrcIP:    srcAddr.IP,
			DstIP:    dstAddr.IP,
		}
	} else {
		ipLayer = &layers.IPv6{
			Version:    6,
			HopLimit:   64,
			NextHeader: layers.IPProtocolUDP,
			SrcIP:      srcAddr.IP,
			DstIP:      dstAddr.IP,
		}
		ethLayer.EthernetType = layers.EthernetTypeIPv6
	}

	udpLayer := &layers.UDP{
		SrcPort: layers.UDPPort(srcAddr.Port),
		DstPort: layers.UDPPort(dstAddr.Port),
	}
	udpLayer.SetNetworkLayerForChecksum(ipLayer.(gopacket.NetworkLayer))

	// Serialize packet
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}

	err := gopacket.SerializeLayers(buffer, options,
		ethLayer,
		ipLayer,
		udpLayer,
		gopacket.Payload(payload),
	)
	if err != nil {
		return fmt.Errorf("failed to serialize packet: %v", err)
	}

	// Write to PCAP
	return session.pcapWriter.WritePacket(gopacket.CaptureInfo{
		Timestamp:     time.Now(),
		CaptureLength: len(buffer.Bytes()),
		Length:        len(buffer.Bytes()),
	}, buffer.Bytes())
}
