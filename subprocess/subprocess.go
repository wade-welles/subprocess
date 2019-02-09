package subprocess

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/kr/pty"
	"github.com/pkg/errors"
)

var ErrTimeout = errors.New("timeout expecting results")

const DefaultTimeout = 30 * time.Second

type SubProcess struct {
	command *exec.Cmd
	ctx     context.Context
	pty     *os.File
	log     *logger
}

func NewSubProcess(command string, args ...string) (*SubProcess, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, command, args...)

	return &SubProcess{
		command: cmd,
		log:     &logger{},
		ctx:     ctx,
	}, nil
}

func (s *SubProcess) listenForShutdown(wg *sync.WaitGroup, signals chan os.Signal, errs chan error, cancel context.CancelFunc) {
	defer wg.Done()

	for {
		select {
		case e := <-errs:
			log.Printf("failed with error: %v", e)
			cancel()
			return

		case sig := <-signals:
			switch sig {
			case syscall.SIGWINCH:
				if err := pty.InheritSize(os.Stdin, s.pty); err != nil {
					// probably not worth shutting down the process over this error, so let's log and move on
					log.Printf("error resizing pty: %s", err)
				}

			case os.Interrupt:
				fallthrough
			case syscall.SIGTSTP:
				fallthrough
			case syscall.SIGINT:
				cancel()
				return
			}
		}
	}
}

func waitForCommandCompletion(ctx context.Context, wg *sync.WaitGroup, cmd *exec.Cmd, errs chan error) {
	defer wg.Done()

	done := make(chan error)

	go func() {
		err := cmd.Wait()
		if err != nil {
			errs <- err
		}
		close(done)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		}
	}
}

func copyFrom(ctx context.Context, wg *sync.WaitGroup, dst io.Writer, src io.Reader, errs chan error) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			go func() {
				_, err := io.Copy(dst, src)
				if err != nil {
					log.Printf("unable to copy pty to stdout: %v", err)
					errs <- err
				}
			}()
		}
	}
}

func (s *SubProcess) Interact() error {
	errs := make(chan error)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGINT, syscall.SIGWINCH, syscall.SIGTSTP)

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(s.ctx)

	wg.Add(1)
	go s.listenForShutdown(&wg, signals, errs, cancel)

	wg.Add(1)
	go waitForCommandCompletion(ctx, &wg, s.command, errs)

	wg.Add(1)
	go copyFrom(ctx, &wg, os.Stdout, s.pty, errs)

	wg.Add(1)
	go copyFrom(ctx, &wg, s.pty, os.Stdin, errs)

	wg.Wait()
	if len(s.log.log.String()) > 0 {
		fmt.Println("\nlog: ", s.log.log.String())
	}

	return nil
}

func (s *SubProcess) Start() error {
	p, err := pty.Start(s.command)
	if err != nil {
		return err
	}
	s.pty = p

	return nil
}

func (s *SubProcess) Close() error {
	return s.command.Process.Kill()
}

func (s *SubProcess) Send(value string) error {
	_, err := s.pty.Write([]byte(value))
	return err
}

func (s *SubProcess) SendLine(value string) error {
	return s.Send(value + "\r\n")
}

func (s *SubProcess) ExpectWithTimeout(expression *regexp.Regexp, duration time.Duration) (bool, error) {
	expressions := []*regexp.Regexp{
		expression,
	}
	index, err := s.ExpectExpressionsWithTimeout(expressions, duration)
	return index == 0, err
}

func (s *SubProcess) Expect(expression *regexp.Regexp) (bool, error) {
	return s.ExpectWithTimeout(expression, DefaultTimeout)
}

func (s *SubProcess) ExpectExpressions(expressions []*regexp.Regexp) (int, error) {
	return s.ExpectExpressionsWithTimeout(expressions, DefaultTimeout)
}

func (s *SubProcess) readOutput(ctx context.Context, wg *sync.WaitGroup, buf io.Writer, lock *sync.RWMutex, errs chan error) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			var temp bytes.Buffer

			n, err := io.Copy(&temp, s.pty)
			if err != nil {
				if err != io.EOF {
					errs <- err
					close(errs)
					return
				}
			}

			if n > 0 {
				lock.Lock()
				_, _ = buf.Write(temp.Bytes())
				fmt.Println("read: ", string(temp.Bytes()))
				lock.Unlock()
			}
		}
	}
}

func (s *SubProcess) ExpectExpressionsWithTimeout(expressions []*regexp.Regexp, timeout time.Duration) (int, error) {
	errs := make(chan error, 1)
	ctx, _ := context.WithDeadline(context.Background(), time.Now().Add(timeout))

	var output bytes.Buffer
	var rwLock sync.RWMutex

	var wg sync.WaitGroup

	wg.Add(1)
	go s.readOutput(ctx, &wg, &output, &rwLock, errs)

	var index = -1
	var e error

OUTER:
	for {
		select {
		case <-ctx.Done():
			e = ErrTimeout
			break OUTER

		case err := <-errs:
			s.log.Printf("error reading from pty: %v", err)
			e = errors.Wrap(err, "error reading from pty")
			break OUTER

		case <-time.After(50 * time.Microsecond): // TODO: adjust this
			rwLock.RLock()
			b := output.Bytes()
			rwLock.RUnlock()

			for i, r := range expressions {
				if r.Find(b) != nil {
					index = i
					break OUTER
				}
			}
		}
	}

	//wg.Wait()
	return index, e
}
