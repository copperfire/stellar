package kelpos

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"log"
	"os/exec"
)

// StreamOutput runs the provided command in a streaming fashion
func (kos *KelpOS) StreamOutput(command *exec.Cmd) error {
	stdout, e := command.StdoutPipe()
	if e != nil {
		return fmt.Errorf("error while creating Stdout pipe: %s", e)
	}
	command.Start()

	scanner := bufio.NewScanner(stdout)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("\t%s\n", line)
	}

	e = command.Wait()
	if e != nil {
		return fmt.Errorf("could not execute command: %s", e)
	}
	return nil
}

// SafeUnregister ignores erros when unregistering the command at the provided namespace
func (kos *KelpOS) SafeUnregister(namespace string) {
	kos.Unregister(namespace)
}

// Stop unregisters and stops the command at the provided namespace
func (kos *KelpOS) Stop(namespace string) error {
	if p, exists := kos.GetProcess(namespace); exists {
		e := kos.Unregister(namespace)
		if e != nil {
			return fmt.Errorf("could not stop command because of an error when unregistering command for namespace '%s': %s", namespace, e)
		}

		log.Printf("killing process %d\n", p.Cmd.Process.Pid)
		return p.Cmd.Process.Kill()
	}
	return fmt.Errorf("process with namespace does not exist: %s", namespace)
}

// Blocking runs a bash command and blocks
func (kos *KelpOS) Blocking(namespace string, cmd string) ([]byte, error) {
	p, e := kos.Background(namespace, cmd)
	if e != nil {
		return nil, fmt.Errorf("could not run bash command in background '%s': %s", cmd, e)
	}

	var outputBytes []byte
	var err error
	go func() {
		outputBytes, err = ioutil.ReadAll(p.Stdout)
	}()

	e = p.Cmd.Wait()
	if e != nil {
		return nil, fmt.Errorf("error waiting for bash command '%s': %s", cmd, e)
	}

	e = kos.Unregister(namespace)
	if e != nil {
		return nil, fmt.Errorf("error unregistering bash command '%s': %s", cmd, e)
	}

	return outputBytes, err
}

// Background runs the provided bash command in the background and registers the command
func (kos *KelpOS) Background(namespace string, cmd string) (*Process, error) {
	c := exec.Command("bash", "-c", cmd)

	stdinWriter, e := c.StdinPipe()
	if e != nil {
		return nil, fmt.Errorf("could not get Stdin pipe for bash command '%s': %s", cmd, e)
	}
	stdoutReader, e := c.StdoutPipe()
	if e != nil {
		return nil, fmt.Errorf("could not get Stdout pipe for bash command '%s': %s", cmd, e)
	}

	e = c.Start()
	if e != nil {
		return nil, fmt.Errorf("could not start bash command '%s': %s", cmd, e)
	}

	p := &Process{
		Cmd:    c,
		Stdin:  stdinWriter,
		Stdout: stdoutReader,
	}
	e = kos.register(namespace, p)
	if e != nil {
		return nil, fmt.Errorf("error registering bash command '%s': %s", cmd, e)
	}

	return p, nil
}

func (kos *KelpOS) register(namespace string, p *Process) error {
	kos.processLock.Lock()
	defer kos.processLock.Unlock()

	if _, exists := kos.processes[namespace]; exists {
		return fmt.Errorf("process with namespace already exists: %s", namespace)
	}

	kos.processes[namespace] = *p
	log.Printf("registered command under namespace '%s' with PID: %d", namespace, p.Cmd.Process.Pid)
	return nil
}

// Unregister unregisters the command at the provided namespace, returning an error if needed
func (kos *KelpOS) Unregister(namespace string) error {
	kos.processLock.Lock()
	defer kos.processLock.Unlock()

	if p, exists := kos.processes[namespace]; exists {
		delete(kos.processes, namespace)
		log.Printf("unregistered command under namespace '%s' with PID: %d", namespace, p.Cmd.Process.Pid)
		return nil
	}
	return fmt.Errorf("process with namespace does not exist: %s", namespace)
}

// GetProcess gets the process tied to the provided namespace
func (kos *KelpOS) GetProcess(namespace string) (*Process, bool) {
	kos.processLock.Lock()
	defer kos.processLock.Unlock()

	p, exists := kos.processes[namespace]
	return &p, exists
}
