package relay

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
)

var errNoFreePort = errors.New("no free port in range")

// portAllocator hands out external ports. Only the relay chooses them: hosts cannot ask for one.
// It is guarded by Server.mu.
type portAllocator struct {
	bind     string
	min, max int
	used     map[int]bool   // ports bound by live tunnels
	reserved map[int]string // fixed ports from the config: port → user
}

// listen binds a player listener for user: the preferred ports first, in order (the user's
// fixed port, or its previous and its home port), then a random free port outside avoid, then
// any free port. Ports reserved for other users are never handed out.
func (a *portAllocator) listen(user string, preferred []int, avoid map[int]bool) (*net.TCPListener, int, error) {
	for _, p := range preferred {
		if ln := a.try(user, p); ln != nil {
			return ln, p, nil
		}
	}
	n := a.max - a.min + 1
	start := rand.IntN(n)
	for pass := 0; pass < 2; pass++ { // first the ports nobody else wants, then the rest
		for i := 0; i < n; i++ {
			p := a.min + (start+i)%n
			if avoid[p] != (pass == 1) {
				continue
			}
			if ln := a.try(user, p); ln != nil {
				return ln, p, nil
			}
		}
	}
	return nil, 0, errNoFreePort
}

func (a *portAllocator) try(user string, p int) *net.TCPListener {
	if p < a.min || p > a.max || a.used[p] {
		return nil
	}
	if owner, ok := a.reserved[p]; ok && owner != user {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(a.bind, strconv.Itoa(p)))
	if err != nil { // taken by another process
		return nil
	}
	a.used[p] = true
	return ln.(*net.TCPListener)
}

func (a *portAllocator) release(p int) {
	delete(a.used, p)
}

// derivedPort is the port a hash of the user name points to in portMin..portMax: it needs no
// stored state, so it stays the same across relay restarts.
func derivedPort(user string, portMin, portMax int) int {
	h := fnv.New32a()
	h.Write([]byte(user))
	return portMin + int(h.Sum32()%uint32(portMax-portMin+1))
}

// HomePorts returns the home port of every user without a fixed port: the port derived from
// its name, or, when that one is a fixed port or already another user's home port, the next
// port up (wrapping around to portMin) that is neither. Names are handled in sorted order, so
// on a collision the name that sorts first keeps the derived port. The result depends only on
// the user list, not on which host connects first, so every address survives a relay restart.
// Disabled users count like the others: they keep their ports, so switching a user off or on
// moves nobody else. A user is missing from the result only when the range has no such port
// left for it.
func HomePorts(users []User, portMin, portMax int) map[string]int {
	return homePorts(userMap(users), portMin, portMax)
}

// userMap indexes users by name; a later entry with the same name wins, as in SetUsers.
func userMap(users []User) map[string]*User {
	m := make(map[string]*User, len(users))
	for i := range users {
		m[users[i].Name] = &users[i]
	}
	return m
}

func homePorts(users map[string]*User, portMin, portMax int) map[string]int {
	taken := make(map[int]bool) // fixed ports and the home ports handed out so far
	var names []string
	for name, u := range users {
		if u.Port != 0 {
			taken[u.Port] = true
		} else {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	n := portMax - portMin + 1
	home := make(map[string]int, len(names))
	for _, name := range names {
		d := derivedPort(name, portMin, portMax) - portMin
		for i := 0; i < n; i++ {
			if p := portMin + (d+i)%n; !taken[p] {
				home[name] = p
				taken[p] = true
				break
			}
		}
	}
	return home
}

// PortWarnings describes, one line per user, every enabled user without a fixed port whose
// tunnels do not get the port derived from its name (see HomePorts), and which port they get
// instead. A disabled user that holds the derived port is named as such. It is empty when every
// such user keeps its derived port.
func PortWarnings(users []User, portMin, portMax int) []string {
	m := userMap(users)
	home := homePorts(m, portMin, portMax)
	fixedBy := make(map[int]string)
	homeOf := make(map[int]string)
	var names []string
	for name, u := range m {
		if u.Port != 0 {
			fixedBy[u.Port] = name
		} else if !u.Disabled { // a disabled user has no tunnels whose address could surprise
			names = append(names, name)
		}
	}
	for name, p := range home {
		homeOf[p] = name
	}
	owner := func(name string) string {
		if m[name].Disabled {
			return fmt.Sprintf("%q (disabled, but disabled users keep their ports)", name)
		}
		return strconv.Quote(name)
	}
	slices.Sort(names)
	var out []string
	for _, name := range names {
		d := derivedPort(name, portMin, portMax)
		p, ok := home[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("user %q gets no port of its own: every port in %d-%d is a fixed port or another user's; its tunnels take any free port, so its address can change",
				name, portMin, portMax))
		case p == d:
		case fixedBy[d] != "":
			out = append(out, fmt.Sprintf("user %q gets port %d: port %d, derived from its name, is the fixed port of %s",
				name, p, d, owner(fixedBy[d])))
		default:
			out = append(out, fmt.Sprintf("user %q gets port %d: port %d, derived from its name, goes to %s, whose name sorts first",
				name, p, d, owner(homeOf[d])))
		}
	}
	return out
}
