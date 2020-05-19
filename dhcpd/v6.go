package dhcpd

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/AdguardTeam/golibs/log"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/server6"
	"github.com/insomniacslk/dhcp/iana"
)

const valueIAID = "ADGH" // value for IANA.ID

// V6Server - DHCPv6 server
type V6Server struct {
	srv        *server6.Server
	leases     []*Lease
	leasesLock sync.Mutex

	conf V6ServerConf
}

// V6ServerConf - server configuration
type V6ServerConf struct {
	Enabled       bool   `yaml:"enabled"`
	InterfaceName string `yaml:"interface_name"`
	RangeStart    string `yaml:"range_start"`
	LeaseDuration uint32 `yaml:"lease_duration"` // in seconds

	ipStart    net.IP
	leaseTime  time.Duration
	dnsIPAddrs []net.IP // IPv6 addresses to return to DHCP clients as DNS server addresses
	sid        dhcpv6.Duid

	notify func(uint32)
}

// WriteDiskConfig - write configuration
func (s *V6Server) WriteDiskConfig(c *V6ServerConf) {
	*c = s.conf
}

// ResetLeases - reset leases
func (s *V6Server) ResetLeases(ll []*Lease) {
	s.leases = nil
	for _, l := range ll {
		// TODO
		s.leases = append(s.leases, l)
	}
}

// GetLeases - get current leases
func (s *V6Server) GetLeases(flags int) []Lease {
	var result []Lease
	s.leasesLock.Lock()
	for _, lease := range s.leases {

		if lease.Expiry.Unix() == leaseExpireStatic {
			if (flags & LeasesStatic) != 0 {
				result = append(result, *lease)
			}

		} else {
			if (flags & LeasesDynamic) != 0 {
				result = append(result, *lease)
			}
		}
	}
	s.leasesLock.Unlock()
	return result
}

// FindMACbyIP6 - find a MAC address by IP address in the currently active DHCP leases
func (s *V6Server) FindMACbyIP6(ip net.IP) net.HardwareAddr {
	now := time.Now().Unix()

	s.leasesLock.Lock()
	defer s.leasesLock.Unlock()

	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}

	for _, l := range s.leases {
		if l.IP.Equal(ip4) {
			unix := l.Expiry.Unix()
			if unix > now || unix == leaseExpireStatic {
				return l.HWAddr
			}
		}
	}
	return nil
}

// AddStaticLease - add a static lease
func (s *V6Server) AddStaticLease(l Lease) error {
	if len(l.IP) != 16 {
		return fmt.Errorf("invalid IP")
	}
	if len(l.HWAddr) != 6 {
		return fmt.Errorf("invalid MAC")
	}

	l.Expiry = time.Unix(leaseExpireStatic, 0)

	s.leasesLock.Lock()
	err := s.addLease(l)
	if err != nil {
		s.leasesLock.Unlock()
		return err
	}
	s.conf.notify(LeaseChangedDBStore)
	s.leasesLock.Unlock()
	s.conf.notify(LeaseChangedAddedStatic)
	return nil
}

// RemoveStaticLease - remove a static lease
func (s *V6Server) RemoveStaticLease(l Lease) error {
	if len(l.IP) != 16 {
		return fmt.Errorf("invalid IP")
	}
	if len(l.HWAddr) != 6 {
		return fmt.Errorf("invalid MAC")
	}

	s.leasesLock.Lock()
	err := s.rmLease(l)
	if err != nil {
		s.leasesLock.Unlock()
		return err
	}
	s.conf.notify(LeaseChangedDBStore)
	s.leasesLock.Unlock()
	s.conf.notify(LeaseChangedRemovedStatic)
	return nil
}

// Add a lease
func (s *V6Server) addLease(l Lease) error {
	for _, it := range s.leases {
		if net.IP.Equal(it.IP, l.IP) ||
			bytes.Equal(it.HWAddr, l.HWAddr) {
			return fmt.Errorf("Lease already exists")
		}
	}
	s.leases = append(s.leases, &l)
	return nil
}

// Remove a lease
func (s *V6Server) rmLease(l Lease) error {
	var newLeases []*Lease
	for _, lease := range s.leases {
		if net.IP.Equal(lease.IP, l.IP) {
			if !bytes.Equal(lease.HWAddr, l.HWAddr) {
				return fmt.Errorf("Lease not found")
			}
			continue
		}
		newLeases = append(newLeases, lease)
	}

	if len(newLeases) == len(s.leases) {
		return fmt.Errorf("Lease not found: %s", l.IP)
	}

	s.leases = newLeases
	return nil
}

// Find lease by MAC
func (s *V6Server) findLease(mac net.HardwareAddr) *Lease {
	s.leasesLock.Lock()
	defer s.leasesLock.Unlock()

	for i := range s.leases {
		if bytes.Equal(mac, s.leases[i].HWAddr) {
			return s.leases[i]
		}
	}
	return nil
}

// Reserve lease for MAC
func (s *V6Server) reserveLease(mac net.HardwareAddr) *Lease {
	l := Lease{}
	l.HWAddr = make([]byte, 6)
	copy(l.HWAddr, mac)
	l.IP = make([]byte, 16)

	s.leasesLock.Lock()
	defer s.leasesLock.Unlock()

	copy(l.IP, s.conf.ipStart)
	if s.conf.ipStart[15] == 0xff {
		return nil
	}
	s.conf.ipStart[15]++

	err := s.addLease(l)
	if err != nil {
		return nil
	}
	return &l
}

// Check Client ID
func (s *V6Server) checkCID(msg *dhcpv6.Message) error {
	if msg.Options.ClientID() == nil {
		return fmt.Errorf("DHCPv6: no ClientID option in request")
	}
	return nil
}

// Check ServerID policy
func (s *V6Server) checkSID(msg *dhcpv6.Message) error {
	sid := msg.Options.ServerID()

	switch msg.Type() {
	case dhcpv6.MessageTypeSolicit,
		dhcpv6.MessageTypeConfirm,
		dhcpv6.MessageTypeRebind:

		if sid != nil {
			return fmt.Errorf("DHCPv6: drop packet: ServerID option in message %s", msg.Type().String())
		}

	case dhcpv6.MessageTypeRequest,
		dhcpv6.MessageTypeRenew,
		dhcpv6.MessageTypeRelease,
		dhcpv6.MessageTypeDecline:

		if sid == nil {
			return fmt.Errorf("DHCPv6: drop packet: no ServerID option in message %s", msg.Type().String())
		}
		if !sid.Equal(s.conf.sid) {
			return fmt.Errorf("DHCPv6: drop packet: mismatched ServerID option in message %s: %s",
				msg.Type().String(), sid.String())
		}
	}

	return nil
}

// . IAID must be equal to this server's ID
// . IAAddress must be equal to the lease's IP
func (s *V6Server) checkIA(msg *dhcpv6.Message, lease *Lease) error {
	switch msg.Type() {
	case dhcpv6.MessageTypeRequest,
		dhcpv6.MessageTypeConfirm,
		dhcpv6.MessageTypeRenew,
		dhcpv6.MessageTypeRebind:

		oia := msg.Options.OneIANA()
		if oia == nil {
			return fmt.Errorf("no IANA option in %s", msg.Type().String())
		}

		if !bytes.Equal(oia.IaId[:], []byte(valueIAID)) {
			return fmt.Errorf("invalid IANA.ID value in %s", msg.Type().String())
		}

		oiaAddr := oia.Options.OneAddress()
		if oiaAddr == nil {
			return fmt.Errorf("no IANA.Addr option in %s", msg.Type().String())
		}

		if !oiaAddr.IPv6Addr.Equal(lease.IP) {
			return fmt.Errorf("invalid IANA.Addr option in %s", msg.Type().String())
		}
	}
	return nil
}

// Store lease in DB (if necessary) and return lease life time
func (s *V6Server) commitLease(msg *dhcpv6.Message, lease *Lease) time.Duration {
	lifetime := s.conf.leaseTime

	switch msg.Type() {
	case dhcpv6.MessageTypeSolicit:
		//

	case dhcpv6.MessageTypeConfirm:
		lifetime = lease.Expiry.Sub(time.Now())

	case dhcpv6.MessageTypeRequest,
		dhcpv6.MessageTypeRenew,
		dhcpv6.MessageTypeRebind:

		if lease.Expiry.Unix() != leaseExpireStatic {

			lease.Expiry = time.Now().Add(s.conf.leaseTime)

			s.leasesLock.Lock()
			s.conf.notify(LeaseChangedDBStore)
			s.leasesLock.Unlock()
			s.conf.notify(LeaseChangedAdded)
		}
	}
	return lifetime
}

// Find a lease associated with MAC and prepare response
func (s *V6Server) process(msg *dhcpv6.Message, req dhcpv6.DHCPv6, resp dhcpv6.DHCPv6) bool {
	switch msg.Type() {
	case dhcpv6.MessageTypeSolicit,
		dhcpv6.MessageTypeRequest,
		dhcpv6.MessageTypeConfirm,
		dhcpv6.MessageTypeRenew,
		dhcpv6.MessageTypeRebind:
		// continue

	default:
		return false
	}

	mac, err := dhcpv6.ExtractMAC(req)
	if err != nil {
		log.Debug("DHCPv6: dhcpv6.ExtractMAC: %s", err)
		return false
	}

	lease := s.findLease(mac)
	if lease == nil {
		log.Debug("DHCPv6: no lease for: %s", mac)

		switch msg.Type() {

		case dhcpv6.MessageTypeSolicit:
			lease = s.reserveLease(mac)
			if lease == nil {
				return false
			}

		default:
			return false
		}
	}

	err = s.checkIA(msg, lease)
	if err != nil {
		log.Debug("DHCPv6: %s", mac)
		return false
	}

	lifetime := s.commitLease(msg, lease)

	oia := &dhcpv6.OptIANA{}
	copy(oia.IaId[:], []byte(valueIAID))
	oiaAddr := &dhcpv6.OptIAAddress{
		IPv6Addr:          lease.IP,
		PreferredLifetime: lifetime,
		ValidLifetime:     lifetime,
	}
	oia.Options = dhcpv6.IdentityOptions{
		Options: []dhcpv6.Option{oiaAddr},
	}
	resp.AddOption(oia)

	if msg.IsOptionRequested(dhcpv6.OptionDNSRecursiveNameServer) {
		resp.UpdateOption(dhcpv6.OptDNS(s.conf.dnsIPAddrs...))
	}
	return true
}

// 1.
// fe80::* (client) --(Solicit + ClientID+IANA())-> ff02::1:2
// server -(Advertise + ClientID+ServerID+IANA(IAAddress)> fe80::*
// fe80::* --(Request + ClientID+ServerID+IANA(IAAddress))-> ff02::1:2
// server -(Reply + ClientID+ServerID+IANA(IAAddress)+DNS)> fe80::*
//
// 2.
// fe80::* --(Confirm|Renew|Rebind + ClientID+IANA(IAAddress))-> ff02::1:2
// server -(Reply + ClientID+ServerID+IANA(IAAddress)+DNS)> fe80::*
//
// 3.
// fe80::* --(Release + ClientID+ServerID+IANA(IAAddress))-> ff02::1:2
func (s *V6Server) packetHandler(conn net.PacketConn, peer net.Addr, req dhcpv6.DHCPv6) {
	msg, err := req.GetInnerMessage()
	if err != nil {
		log.Error("DHCPv6: %s", err)
		return
	}

	log.Debug("DHCPv6: received: %s", req.Summary())

	err = s.checkCID(msg)
	if err != nil {
		log.Debug("%s", err)
		return
	}

	err = s.checkSID(msg)
	if err != nil {
		log.Debug("%s", err)
		return
	}

	var resp dhcpv6.DHCPv6

	switch msg.Type() {
	case dhcpv6.MessageTypeSolicit:
		if msg.GetOneOption(dhcpv6.OptionRapidCommit) == nil {
			resp, err = dhcpv6.NewAdvertiseFromSolicit(msg)
			break
		}
		fallthrough

	case dhcpv6.MessageTypeRequest,
		dhcpv6.MessageTypeConfirm,
		dhcpv6.MessageTypeRenew,
		dhcpv6.MessageTypeRebind,
		dhcpv6.MessageTypeRelease,
		dhcpv6.MessageTypeInformationRequest:
		resp, err = dhcpv6.NewReplyFromMessage(msg)

	default:
		log.Error("DHCPv6: message type %d not supported", msg.Type())
		return
	}

	if err != nil {
		log.Error("DHCPv6: %s", err)
		return
	}

	resp.AddOption(dhcpv6.OptServerID(s.conf.sid))

	_ = s.process(msg, req, resp)

	log.Debug("DHCPv6: sending: %s", resp.Summary())

	_, err = conn.WriteTo(resp.ToBytes(), peer)
	if err != nil {
		log.Error("DHCPv6: conn.Write to %s failed: %s", peer, err)
		return
	}
}

// Get IPv6 address list
func getIfaceIPv6(iface net.Interface) []net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}

	var res []net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.To4() == nil {
			res = append(res, ipnet.IP)
		}
	}
	return res
}

// Start - start server
func (s *V6Server) Start() error {
	iface, err := net.InterfaceByName(s.conf.InterfaceName)
	if err != nil {
		return wrapErrPrint(err, "Couldn't find interface by name %s", s.conf.InterfaceName)
	}

	if !s.conf.Enabled {
		return nil
	}

	log.Debug("DHCPv6: starting...")
	s.conf.dnsIPAddrs = getIfaceIPv6(*iface)
	if len(s.conf.dnsIPAddrs) == 0 {
		return fmt.Errorf("DHCPv6: no IPv6 address for interface %s", iface.Name)
	}

	if len(iface.HardwareAddr) != 6 {
		return fmt.Errorf("DHCPv6: invalid MAC %s", iface.HardwareAddr)
	}
	s.conf.sid = dhcpv6.Duid{
		Type:          dhcpv6.DUID_LLT,
		HwType:        iana.HWTypeEthernet,
		LinkLayerAddr: iface.HardwareAddr,
	}

	laddr := &net.UDPAddr{
		IP:   net.ParseIP("::"),
		Port: dhcpv6.DefaultServerPort,
	}
	server, err := server6.NewServer(iface.Name, laddr, s.packetHandler, server6.WithDebugLogger())
	if err != nil {
		return err
	}

	go func() {
		err = server.Serve()
		log.Error("DHCPv6: %s", err)
	}()
	return nil
}

// Reset - stop server
func (s *V6Server) Reset() {
	s.leasesLock.Lock()
	s.leases = nil
	s.leasesLock.Unlock()
}

// Stop - stop server
func (s *V6Server) Stop() {
	if s.srv == nil {
		return
	}

	err := s.srv.Close()
	if err != nil {
		log.Error("DHCPv6: srv.Close: %s", err)
	}
	// now server.Serve() will return
}

// Create DHCPv6 server
func v6Create(conf V6ServerConf) (*V6Server, error) {
	s := &V6Server{}
	s.conf = conf

	if !conf.Enabled {
		return s, nil
	}

	s.conf.ipStart = net.ParseIP(conf.RangeStart)
	if s.conf.ipStart == nil {
		return nil, fmt.Errorf("DHCPv6: invalid range-start IP: %s", conf.RangeStart)
	}

	if conf.LeaseDuration == 0 {
		s.conf.leaseTime = time.Hour * 24
		s.conf.LeaseDuration = uint32(s.conf.leaseTime.Seconds())
	} else {
		s.conf.leaseTime = time.Second * time.Duration(conf.LeaseDuration)
	}

	return s, nil
}