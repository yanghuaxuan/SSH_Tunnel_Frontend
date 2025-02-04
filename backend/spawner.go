package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"time"
	"math/rand"
)

const AUTOREBOOT_TIMEOUT = time.Second * 30

type TunnelStatus int

const (
	Disconnected TunnelStatus = iota
	Loading
	Online
)

type Tunnel struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Local_port  int    `json:"local_port"`
	Host        string `json:"host"`
	Remote_port int    `json:"remote_port"`
	Conn_addr   string `json:"conn_addr"`
	Autoreboot  bool   `json:"autoreboot"`
}

type Tunnel_Process struct {
	cmd             *exec.Cmd
	tunnel          Tunnel
	status          TunnelStatus
	autoreboot_chan chan bool
}

type Spawner struct {
	tunnels 	map[string]Tunnel
	procs   	map[string]*Tunnel_Process
	ssh_path	string
}

/* try to get SSH executable */
func try_ssh() (string, error) {
	p, err := exec.LookPath("ssh")
	if (err == nil) {
		return p, nil
	}
	p, err = exec.LookPath("/usr/bin/ssh")
	if (err == nil) {
		return p, nil
	}
	p, err = exec.LookPath("/data/data/com.termux/files/usr/bin/ssh")
	if (err == nil) {
		return p, nil
	}
	return "", err
}

/* stops a tunnel, if it exists in proc */
func (s *Spawner) stop_tunnel(tunId string) {
	tun, exists := s.tunnels[tunId]
	if (!exists) {
		return
	}
	p, exists := s.procs[tunId]
	if !exists {
		return
	}
	p.autoreboot_chan <- false
	delete(s.procs, tunId)
	if p.cmd.Process != nil {
		err := p.cmd.Process.Kill()
		if (err != nil) {
			slog.Debug(fmt.Sprintf("Cannot kill %s's SSH session: %s", tun.Name, err))
		} else {
			p.cmd.Wait()
		}
	}
}

/* use this to properly start a tunnel */
func (s *Spawner) start_tunnel(tunId string) {
	tun, exists := s.tunnels[tunId]
	if (!exists) {
		return
	}

    proc := kickstart(tun, s.ssh_path)
    s.procs[tunId] = &proc
    go track_exit(&proc)
    if tun.Autoreboot {
        go auto_reboot_on_sig(&proc, s.ssh_path)
    }
}

/* very basic id builder */
func genId(n int) string {
	const alphanumeric = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range n {
		b[i] = alphanumeric[rand.Intn(len(alphanumeric))]
	}
	return string(b)
}

func init_spawner(tun []Tunnel, ssh_path string) Spawner {
	tun_map := make(map[string]Tunnel)
	proc_map := make(map[string]*Tunnel_Process)

    s := Spawner{tun_map, proc_map, ssh_path}

	for i := range tun {
		t := tun[i]
		s.tunnels[t.Id] = t
		if t.Enabled {
            s.start_tunnel(t.Id)
		}
	}

	return Spawner{tun_map, proc_map, "ssh"}
}

func track_exit(tun *Tunnel_Process) {
	if tun == nil {
		return
	}

	tun.cmd.Wait()
	slog.Debug("SSH session exited!")
	tun.status = Disconnected
	tun.autoreboot_chan <- true
}

func log_tunnel(tun Tunnel, rc io.ReadCloser) {
	buf := bufio.NewReader(rc)
	for {
		line, err := buf.ReadString('\n')
		/* it's probably a good idea to close early indiscriminately to avoid spamming the logs */
		if err != nil{
			slog.Debug(fmt.Sprintf("Error while reading stderr from %s: %s", tun.Name, err))
			break
		} else {
			slog.Warn(fmt.Sprintf("Message from %s ->\n%s", tun.Name, line))
		}
	}
}

/* attempts to start the SSH process if autoreboot_chan received a true value.  */
func auto_reboot_on_sig(proc *Tunnel_Process, ssh_path string) {
	if proc == nil {
		return
	}

	s := <-proc.autoreboot_chan
	if !s {
		slog.Debug("Exiting autoreboot!")
		return
	}

	slog.Debug("Autorebooting!")

	tun := proc.tunnel
	cmd := exec.Command(ssh_path, "-o", "ExitOnForwardFailure=yes", "-N", "-L", fmt.Sprintf("%d:%s:%d", tun.Local_port, tun.Host, tun.Remote_port), tun.Conn_addr)
	stderr, err := cmd.StderrPipe()
	if (err == nil) {
		go log_tunnel(tun, stderr)
	} else {
		slog.Warn("Cannot log a SSH session!")
	}
	proc.cmd = cmd
	slog.Debug(cmd.String())
	err = cmd.Start()
	proc.status = Online
	if err != nil {
		proc.status = Disconnected
	} else {
		go track_exit(proc)
	}

	time.Sleep(AUTOREBOOT_TIMEOUT)
	go auto_reboot_on_sig(proc, ssh_path)
}

/* start SSH session for tunnel and return its process */
func kickstart(tun Tunnel, ssh_path string) Tunnel_Process {
	cmd := exec.Command(ssh_path, "-o", "ExitOnForwardFailure yes", "-N", "-L", fmt.Sprintf("%d:%s:%d", tun.Local_port, tun.Host, tun.Remote_port), tun.Conn_addr)
	stderr, err := cmd.StderrPipe()
	if (err == nil) {
		go log_tunnel(tun, stderr)
	} else {
		slog.Warn("Cannot log a SSH session!")
	}
	status := Online
	slog.Debug(cmd.String())
	err = cmd.Start()
	if err != nil {
		status = Disconnected
		slog.Warn(fmt.Sprintf("Cannot start SSH session: %s", err))
	}
	/* 2 buffered channel is necessary to avoid deadblocks (i.e. removing a tunnel with no autoreboot) */
	return Tunnel_Process{cmd, tun, status, make(chan bool, 2)}
}
