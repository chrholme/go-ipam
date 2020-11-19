package ipam

import (
	"fmt"
	"math"
	"math/rand"
	"net"
	"time"

	"github.com/avast/retry-go"
	"github.com/pkg/errors"
)

var (
	// ErrNotFound is returned if prefix or cidr was not found
	ErrNotFound NotFoundError
	// ErrNoIPAvailable is returned if no IP is available anymore
	ErrNoIPAvailable NoIPAvailableError
	// ErrIpinUse is retured if IP is already aquired
	ErrIPinUse IPinUseError
)

// Prefix is a expression of a ip with length and forms a classless network.
type Prefix struct {
	Cidr                   string          // The Cidr of this prefix
	ParentCidr             string          // if this prefix is a child this is a pointer back
	availableChildPrefixes map[string]bool // available child prefixes of this prefix
	childPrefixLength      int             // the length of the child prefixes
	Ips                    map[string]bool // The ips contained in this prefix
	version                int64           // version is used for optimistic locking
}

// DeepCopy to a new Prefix
func (p Prefix) DeepCopy() *Prefix {
	return &Prefix{
		Cidr:                   p.Cidr,
		ParentCidr:             p.ParentCidr,
		availableChildPrefixes: copyMap(p.availableChildPrefixes),
		childPrefixLength:      p.childPrefixLength,
		Ips:                    copyMap(p.Ips),
		version:                p.version,
	}
}

func copyMap(m map[string]bool) map[string]bool {
	cm := make(map[string]bool, len(m))
	for k, v := range m {
		cm[k] = v
	}
	return cm
}

// Usage of ips and child Prefixes of a Prefix
type Usage struct {
	AvailableIPs      uint64
	AcquiredIPs       uint64
	AvailablePrefixes uint64
	AcquiredPrefixes  uint64
}

func (i *ipamer) NewPrefix(cidr string, tenantid string) (*Prefix, error) {
	p, err := i.newPrefix(cidr)
	if err != nil {
		return nil, err
	}
	newPrefix, err := i.storage.CreatePrefix(*p, tenantid)
	if err != nil {
		return nil, err
	}

	return &newPrefix, nil
}

func (i *ipamer) DeletePrefix(cidr string, tenantid string) (*Prefix, error) {
	p := i.PrefixFrom(cidr, tenantid)
	if p == nil {
		return nil, fmt.Errorf("%w: delete prefix:%s", ErrNotFound, cidr)
	}
	if len(p.Ips) > 2 {
		return nil, fmt.Errorf("prefix %s has ips, delete prefix not possible", p.Cidr)
	}
	prefix, err := i.storage.DeletePrefix(*p, tenantid)
	if err != nil {
		return nil, fmt.Errorf("delete prefix:%s %v", cidr, err)
	}

	return &prefix, nil
}

func (i *ipamer) AcquireChildPrefix(parentCidr string, length int, tenantid string) (*Prefix, error) {
	var prefix *Prefix
	return prefix, retryOnOptimisticLock(func() error {
		var err error
		prefix, err = i.acquireChildPrefixInternal(parentCidr, length, tenantid)
		return err
	})
}

// acquireChildPrefixInternal will return a Prefix with a smaller length from the given Prefix.
// FIXME allow variable child prefix length
func (i *ipamer) acquireChildPrefixInternal(parentCidr string, length int, tenantid string) (*Prefix, error) {
	prefix := i.PrefixFrom(parentCidr, tenantid)
	if prefix == nil {
		return nil, fmt.Errorf("unable to find prefix for cidr:%s", parentCidr)
	}
	if len(prefix.Ips) > 2 {
		return nil, fmt.Errorf("prefix %s has ips, acquire child prefix not possible", prefix.Cidr)
	}
	ipnet, err := prefix.IPNet()
	if err != nil {
		return nil, err
	}
	ones, size := ipnet.Mask.Size()
	if ones >= length {
		return nil, fmt.Errorf("given length:%d is smaller or equal of prefix length:%d", length, ones)
	}

	// If this is the first call, create a pool of available child prefixes with given length upfront
	if prefix.childPrefixLength == 0 {
		ip := ipnet.IP
		// FIXME use big.Int
		// power of 2 :-(
		// subnetCount := 1 << (uint(length - ones))
		subnetCount := int(math.Pow(float64(2), float64(length-ones)))
		for s := 0; s < subnetCount; s++ {
			ipPart, err := insertNumIntoIP(ip, s, length)
			if err != nil {
				return nil, err
			}
			newIP := &net.IPNet{
				IP:   *ipPart,
				Mask: net.CIDRMask(length, size),
			}
			newCidr := newIP.String()
			child, err := i.newPrefix(newCidr)
			if err != nil {
				return nil, err
			}
			prefix.availableChildPrefixes[child.Cidr] = true

		}
		prefix.childPrefixLength = length
	}
	if prefix.childPrefixLength != length {
		return nil, fmt.Errorf("given length:%d is not equal to existing child prefix length:%d", length, prefix.childPrefixLength)
	}

	var child *Prefix
	for c, available := range prefix.availableChildPrefixes {
		if !available {
			continue
		}
		child, err = i.newPrefix(c)
		if err != nil {
			continue
		}
		break
	}
	if child == nil {
		return nil, fmt.Errorf("no more child prefixes contained in prefix pool")
	}

	prefix.availableChildPrefixes[child.Cidr] = false

	_, err = i.storage.UpdatePrefix(*prefix, tenantid)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to update parent prefix:%v", prefix)
	}
	child, err = i.NewPrefix(child.Cidr, tenantid)
	if err != nil {
		return nil, fmt.Errorf("unable to persist created child:%v", err)
	}
	child.ParentCidr = prefix.Cidr
	_, err = i.storage.UpdatePrefix(*child, tenantid)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to update parent prefix:%v", child)
	}

	return child, nil
}

func (i *ipamer) ReleaseChildPrefix(child *Prefix, tenantid string) error {
	return retryOnOptimisticLock(func() error {
		return i.releaseChildPrefixInternal(child, tenantid)
	})
}

// releaseChildPrefixInternal will mark this child Prefix as available again.
func (i *ipamer) releaseChildPrefixInternal(child *Prefix, tenantid string) error {
	parent := i.PrefixFrom(child.ParentCidr,tenantid)

	if parent == nil {
		return fmt.Errorf("prefix %s is no child prefix", child.Cidr)
	}
	if len(child.Ips) > 2 {
		return fmt.Errorf("prefix %s has ips, deletion not possible", child.Cidr)
	}

	parent.availableChildPrefixes[child.Cidr] = true
	_, err := i.DeletePrefix(child.Cidr, tenantid)
	if err != nil {
		return fmt.Errorf("unable to release prefix %v:%v", child, err)
	}
	_, err = i.storage.UpdatePrefix(*parent, tenantid)
	if err != nil {
		return fmt.Errorf("unable to release prefix %v:%v", child, err)
	}
	return nil
}

func (i *ipamer) PrefixFrom(cidr string, tenantid string) *Prefix {
	prefix, err := i.storage.ReadPrefix(cidr, tenantid)
	if err != nil {
		return nil
	}
	return &prefix
}

func (i *ipamer) AcquireSpecificIP(prefixCidr, specificIP string, tenantid string) (*IP, error) {
	var ip *IP
	return ip, retryOnOptimisticLock(func() error {
		var err error
		ip, err = i.acquireSpecificIPInternal(prefixCidr, specificIP, tenantid)
		return err
	})
}

// acquireSpecificIPInternal will acquire given IP and mark this IP as used, if already in use, return nil.
// If specificIP is empty, the next free IP is returned.
// If there is no free IP an NoIPAvailableError is returned.
// If the Prefix is not found an NotFoundError is returned.
func (i *ipamer) acquireSpecificIPInternal(prefixCidr, specificIP string, tenantid string) (*IP, error) {
	prefix := i.PrefixFrom(prefixCidr, tenantid)
	if prefix == nil {
		return nil, fmt.Errorf("%w: unable to find prefix for cidr:%s", ErrNotFound, prefixCidr)
	}
	if prefix.childPrefixLength > 0 {
		return nil, fmt.Errorf("prefix %s has childprefixes, acquire ip not possible", prefix.Cidr)
	}
	var acquired *IP
	ipnet, err := prefix.IPNet()
	if err != nil {
		return nil, err
	}
	network, err := prefix.Network()
	if err != nil {
		return nil, err
	}

	if specificIP != "" {
		specificIPnet := net.ParseIP(specificIP)
		if specificIPnet == nil {
			return nil, fmt.Errorf("given ip:%s in not valid", specificIP)
		}
		if !ipnet.Contains(specificIPnet) {
			return nil, fmt.Errorf("given ip:%s is not in %s", specificIP, prefixCidr)
		}
	}
    var ipused bool
	for ip := network.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		_, ok := prefix.Ips[ip.String()]
		if ok {
			if specificIP == ip.String() {
				ipused = true
			}
			continue
		}
		if specificIP == "" || specificIP == ip.String() {
			acquired = &IP{
				IP:           ip,
				ParentPrefix: prefix.Cidr,
			}
			prefix.Ips[ip.String()] = true
			_, err := i.storage.UpdatePrefix(*prefix, tenantid)
			if err != nil {
				return nil, errors.Wrapf(err, "unable to persist acquired ip:%v", prefix)
			}
			return acquired, nil
		}
	}
	if ipused {
		return nil, fmt.Errorf("%w: requested ip: %s, already in use.", ErrIPinUse, specificIP )
	}
	return nil, fmt.Errorf("%w: no more ips in prefix: %s left, length of prefix.ips: %d", ErrNoIPAvailable, prefix.Cidr, len(prefix.Ips))
}

func (i *ipamer) AcquireIP(prefixCidr string, tenantid string) (*IP, error) {
	return i.AcquireSpecificIP(prefixCidr, "", tenantid)
}

func (i *ipamer) ReleaseIP(ip *IP, tenantid string) (*Prefix, error) {
	err := i.ReleaseIPFromPrefix(ip.ParentPrefix, ip.IP.String(), tenantid)
	prefix := i.PrefixFrom(ip.ParentPrefix, tenantid)
	return prefix, err
}

func (i *ipamer) ReleaseIPFromPrefix(prefixCidr, ip string, tenantid string) error {
	return retryOnOptimisticLock(func() error {
		return i.releaseIPFromPrefixInternal(prefixCidr, ip, tenantid)
	})
}

// releaseIPFromPrefixInternal will release the given IP for later usage.
func (i *ipamer) releaseIPFromPrefixInternal(prefixCidr, ip string, tenantid string) error {
	prefix := i.PrefixFrom(prefixCidr, tenantid)
	if prefix == nil {
		return fmt.Errorf("%w: unable to find prefix for cidr:%s", ErrNotFound, prefixCidr)
	}
	_, ok := prefix.Ips[ip]
	if !ok {
		return fmt.Errorf("%w: unable to release ip:%s because it is not allocated in prefix:%s", ErrNotFound, ip, prefixCidr)
	}
	delete(prefix.Ips, ip)
	_, err := i.storage.UpdatePrefix(*prefix, tenantid)
	if err != nil {
		return fmt.Errorf("unable to release ip %v:%v", ip, err)
	}
	return nil
}

func (i *ipamer) PrefixesOverlapping(existingPrefixes []string, newPrefixes []string) error {
	for _, ep := range existingPrefixes {
		eip, eipnet, err := net.ParseCIDR(ep)
		if err != nil {
			return fmt.Errorf("parsing prefix %s failed:%v", ep, err)
		}
		for _, np := range newPrefixes {
			nip, nipnet, err := net.ParseCIDR(np)
			if err != nil {
				return fmt.Errorf("parsing prefix %s failed:%v", np, err)
			}
			if eipnet.Contains(nip) || nipnet.Contains(eip) {
				return fmt.Errorf("%s overlaps %s", np, ep)
			}
		}
	}

	return nil
}

// getHostAddresses will return all possible ipadresses a host can get in the given prefix.
// The IPs will be acquired by this method, so that the prefix has no free IPs afterwards.
func (i *ipamer) getHostAddresses(prefix string, tenantid string) ([]string, error) {
	hostAddresses := []string{}

	p, err := i.NewPrefix(prefix, tenantid)
	if err != nil {
		return hostAddresses, err
	}

	// loop till AcquireIP signals that it has no ips left
	for {
		ip, err := i.AcquireIP(p.Cidr, tenantid)
		if errors.Is(err, ErrNoIPAvailable) {
			return hostAddresses, nil
		}
		if err != nil {
			return nil, err
		}
		hostAddresses = append(hostAddresses, ip.IP.String())
	}
}

// newPrefix create a new Prefix from a string notation.
func (i *ipamer) newPrefix(cidr string) (*Prefix, error) {
	_, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("unable to parse cidr:%s %v", cidr, err)
	}
	p := &Prefix{
		Cidr:                   cidr,
		Ips:                    make(map[string]bool),
		availableChildPrefixes: make(map[string]bool),
	}

	broadcast, err := p.broadcast()
	if err != nil {
		return nil, err
	}
	// First IP in the prefix and Broadcast is blocked.
	network, err := p.Network()
	if err != nil {
		return nil, err
	}
	p.Ips[network.String()] = true
	p.Ips[broadcast.IP.String()] = true

	return p, nil
}

func (p *Prefix) broadcast() (*IP, error) {
	ipnet, err := p.IPNet()
	if err != nil {
		return nil, err
	}
	network, err := p.Network()
	if err != nil {
		return nil, err
	}
	mask := ipnet.Mask
	n := IP{IP: network}
	m := IP{IP: net.IP(mask)}

	broadcast := n.or(m.not())
	return &broadcast, nil
}

func (p *Prefix) String() string {
	return p.Cidr
}

func (u *Usage) String() string {
	if u.AvailablePrefixes == uint64(0) {
		return fmt.Sprintf("ip:%d/%d", u.AcquiredIPs, u.AvailableIPs)
	}
	return fmt.Sprintf("ip:%d/%d prefix:%d/%d", u.AcquiredIPs, u.AvailableIPs, u.AcquiredPrefixes, u.AvailablePrefixes)
}

// IPNet return the net.IPNet part of the Prefix
func (p *Prefix) IPNet() (*net.IPNet, error) {
	_, ipnet, err := net.ParseCIDR(p.Cidr)
	return ipnet, err
}

// Network return the net.IP part of the Prefix
func (p *Prefix) Network() (net.IP, error) {
	ip, ipnet, err := net.ParseCIDR(p.Cidr)
	if err != nil {
		return nil, err
	}
	return ip.Mask(ipnet.Mask), nil
}

// availableips return the number of ips available in this Prefix
func (p *Prefix) availableips() uint64 {
	_, ipnet, err := net.ParseCIDR(p.Cidr)
	if err != nil {
		return 0
	}
	var bits int
	if len(ipnet.IP) == net.IPv4len {
		bits = 32
	} else if len(ipnet.IP) == net.IPv6len {
		bits = 128
	}

	ones, _ := ipnet.Mask.Size()
	// FIXME use big.Int
	count := uint64(math.Pow(float64(2), float64(bits-ones)))
	return count
}

// acquiredips return the number of ips acquired in this Prefix
func (p *Prefix) acquiredips() uint64 {
	return uint64(len(p.Ips))
}

// availablePrefixes return the amount of possible prefixes of this prefix if this is a parent prefix
func (p *Prefix) availablePrefixes() uint64 {
	return uint64(len(p.availableChildPrefixes))
}

// acquiredPrefixes return the amount of acquired prefixes of this prefix if this is a parent prefix
func (p *Prefix) acquiredPrefixes() uint64 {
	var count uint64
	for _, available := range p.availableChildPrefixes {
		if !available {
			count++
		}
	}
	return count
}

// Usage report Prefix usage.
func (p *Prefix) Usage() Usage {
	return Usage{
		AvailableIPs:      p.availableips(),
		AcquiredIPs:       p.acquiredips(),
		AvailablePrefixes: p.availablePrefixes(),
		AcquiredPrefixes:  p.acquiredPrefixes(),
	}
}

// NoIPAvailableError indicates that the acquire-operation could not be executed
// because the specified prefix has no free IP anymore.
type NoIPAvailableError struct {
}

func (o NoIPAvailableError) Error() string {
	return "NoIPAvailableError"
}

// NotFoundError is raised if the given Prefix or Cidr was not found
type NotFoundError struct {
}

func (o NotFoundError) Error() string {
	return "NotFound"
}

// IPinUseError is raised if the given IP is already in use so cannot be aquired
type IPinUseError struct {
}
func (o IPinUseError) Error() string {
	return "IPinUseError"
}


// retries the given function if the reported error is an OptimisticLockError
// with ten attempts and jitter delay ~100ms
// returns only error of last failed attempt
func retryOnOptimisticLock(retryableFunc retry.RetryableFunc) error {

	return retry.Do(
		retryableFunc,
		retry.RetryIf(func(err error) bool {
			_, isOptimisticLock := errors.Cause(err).(OptimisticLockError)
			return isOptimisticLock
		}),
		retry.Attempts(10),
		retry.DelayType(JitterDelay),
		retry.LastErrorOnly(true))
}

// jitter will add jitter to a time.Duration.
func jitter(d time.Duration) time.Duration {
	const jitter = 0.50
	jit := 1 + jitter*(rand.Float64()*2-1)
	return time.Duration(jit * float64(d))
}

// JitterDelay is a DelayType which varies delay in each iterations
func JitterDelay(_ uint, config *retry.Config) time.Duration {
	// fields in config are private, so we hardcode the average delay duration
	return jitter(100 * time.Millisecond)
}
