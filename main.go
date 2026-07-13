package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"wifi-cursor/internal/cursor"
	"wifi-cursor/internal/discovery"
	"wifi-cursor/internal/input"
	"wifi-cursor/internal/pool"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "create":
		runCreate()
	case "join":
		runJoin()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`wifi-cursor — общий курсор между устройствами по Wi-Fi.

Команды:
  wifi-cursor create        создать новый пул и получить его ID
  wifi-cursor join [ID]     подключиться к существующему пулу по ID

Пока утилита запущена:
  Перенесите курсор к левому/правому краю экрана, чтобы передать
  управление соседнему устройству в пуле.
  Ctrl+Alt+1..9 — мгновенно переключиться на устройство №N.
  Ctrl+C — выйти из пула.`)
}

func runCreate() {
	backend, err := input.NewBackend()
	fatalIf(err)
	w, h, err := backend.ScreenSize()
	fatalIf(err)

	p := pool.New(hostname(), w, h)
	pid, err := p.CreatePool()
	fatalIf(err)

	engine, err := cursor.New(p, backend)
	fatalIf(err)
	p.SetHandler(engine)

	disc, err := discovery.Open()
	fatalIf(err)
	defer disc.Close()

	fmt.Printf("Пул создан: %s\n", pid)
	fmt.Printf("На другом устройстве в этой же Wi-Fi сети выполните:\n  wifi-cursor join %s\n\n", pid)

	run(p, engine, disc, backend)
}

func runJoin() {
	id := ""
	if len(os.Args) >= 3 {
		id = strings.ToUpper(strings.TrimSpace(os.Args[2]))
	}

	disc, err := discovery.Open()
	fatalIf(err)
	defer disc.Close()

	if id == "" {
		fmt.Println("Ищу активные пулы в этой сети (2 секунды)...")
		found := disc.ScanPresence(context.Background(), 2*time.Second)
		if len(found) > 0 {
			fmt.Println("Найдены пулы:")
			for poolID, d := range found {
				fmt.Printf("  %s  (%s)\n", poolID, d.Name)
			}
		} else {
			fmt.Println("Пока ничего не найдено — можно ввести ID вручную.")
		}
		fmt.Print("Введите ID пула: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		id = strings.ToUpper(strings.TrimSpace(line))
	}
	if id == "" {
		fmt.Println("ID пула не указан")
		os.Exit(1)
	}

	backend, err := input.NewBackend()
	fatalIf(err)
	w, h, err := backend.ScreenSize()
	fatalIf(err)

	p := pool.New(hostname(), w, h)
	engine, err := cursor.New(p, backend)
	fatalIf(err)
	p.SetHandler(engine)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	err = p.JoinPool(ctx, disc, id)
	cancel()
	fatalIf(err)

	fmt.Printf("Подключено к пулу %s\n\n", id)
	run(p, engine, disc, backend)
}

func run(p *pool.Pool, engine *cursor.Engine, disc *discovery.Conn, backend input.Backend) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.Start(ctx, disc)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nВыход из пула...")
		cancel()
	}()

	if err := engine.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
	}
	p.Leave()
	_ = backend.Close()
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "device"
	}
	return h
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
