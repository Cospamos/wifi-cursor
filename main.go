package main

import (
	"bufio"
	"context"
	"flag"
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

// defaultRendezvous is the signaling server used to find pools across the
// internet, where LAN UDP multicast can't reach. It only ever brokers
// introductions; pool traffic (mouse/cursor events) always stays a direct
// connection between the two devices. Override with -server, or -server ""
// to disable it and stay LAN-only.
const defaultRendezvous = "130.61.130.115:47990"

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
	fmt.Println(`wifi-cursor — общий курсор между устройствами по Wi-Fi или интернету.

Команды:
  wifi-cursor create [-server host:port] [-password pass]    создать новый пул
  wifi-cursor join [-server host:port] [-password pass] [ID] подключиться к пулу

По умолчанию используется LAN-обнаружение (та же Wi-Fi сеть) и, если оно не
находит пул, сервер обнаружения на VPS — чтобы можно было подключиться и
через интернет. -server "" отключает сервер и оставляет только LAN.

-password защищает пул общим паролем (без него любой, кто узнает ID —
например, случайным перебором — сможет подключиться). Все устройства пула
должны указать один и тот же пароль.

-invert-scroll переворачивает направление прокрутки колеса при её пересылке
с этого устройства (например, если на нём включена "естественная
прокрутка", а на устройстве назначения — нет).

Пока утилита запущена:
  Перенесите курсор к левому/правому краю экрана, чтобы передать
  управление соседнему устройству в пуле.
  Ctrl+Alt+1..9 — мгновенно переключиться на устройство №N.
  Ctrl+C — выйти из пула.`)
}

func runCreate() {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	server := fs.String("server", defaultRendezvous, `сервер обнаружения (host:port), "" — только локальная сеть`)
	password := fs.String("password", "", "пароль пула (пусто — без пароля)")
	invertScroll := fs.Bool("invert-scroll", false, "перевернуть направление прокрутки при пересылке с этого устройства")
	_ = fs.Parse(os.Args[2:])

	backend, err := input.NewBackend()
	fatalIf(err)
	w, h, err := backend.ScreenSize()
	fatalIf(err)

	p := pool.New(hostname(), w, h)
	pid, err := p.CreatePool(*server, *password)
	fatalIf(err)

	engine, err := cursor.New(p, backend, *invertScroll)
	fatalIf(err)
	p.SetHandler(engine)

	disc, err := discovery.Open()
	fatalIf(err)
	defer disc.Close()

	fmt.Printf("Пул создан: %s\n", pid)
	fmt.Printf("На другом устройстве (в той же Wi-Fi сети или через интернет) выполните:\n  wifi-cursor join %s\n\n", pid)

	run(p, engine, disc, backend)
}

func runJoin() {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	server := fs.String("server", defaultRendezvous, `сервер обнаружения (host:port), "" — только локальная сеть`)
	password := fs.String("password", "", "пароль пула (должен совпадать с тем, что задан на create)")
	invertScroll := fs.Bool("invert-scroll", false, "перевернуть направление прокрутки при пересылке с этого устройства")
	_ = fs.Parse(os.Args[2:])

	id := ""
	if fs.NArg() >= 1 {
		id = strings.ToUpper(strings.TrimSpace(fs.Arg(0)))
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
			fmt.Println("Пока ничего не найдено локально — можно ввести ID вручную (в том числе с другого устройства через интернет).")
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
	engine, err := cursor.New(p, backend, *invertScroll)
	fatalIf(err)
	p.SetHandler(engine)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = p.JoinPool(ctx, disc, *server, id, *password)
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
