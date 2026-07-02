package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// Item represents a single entry in our container/item hierarchy with soft delete support
type Item struct {
	UID         string `json:"uid"`
	Active      bool   `json:"active"`
	Parent      string `json:"parent"`
	Label       string `json:"label"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Qty         int    `json:"qty"`
}

// Inventory handles structural changes, tracking metadata properties alongside elements
type Inventory struct {
	RootUIDs []string `json:"rootuids"`
	LastUID  string   `json:"last_uid"`
	Items    []Item   `json:"items"`
}

// Database encapsulates our in-memory data store, file path, and thread safety
type Database struct {
	mu       sync.RWMutex
	filePath string
	data     Inventory
	mqtt     *mqtt.Server
}

const defaultFilePath = "items.json"

//go:embed index.html
var indexHTML []byte

//go:embed scanner.html
var scannerHTML []byte

// --- CUSTOM MQTT LOGGING HOOK ---
type ConsoleLoggerHook struct {
	mqtt.HookBase
}

func (h *ConsoleLoggerHook) ID() string {
	return "console-logger-hook"
}

func (h *ConsoleLoggerHook) Provides(b byte) bool {
	return b == mqtt.OnPublish || b == mqtt.OnConnectAuthenticate || b == mqtt.OnACLCheck
}

func (h *ConsoleLoggerHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	return true
}

func (h *ConsoleLoggerHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	return true
}

func (h *ConsoleLoggerHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	log.Printf("[MQTT TRAFFIC] Topic: %s | Payload: %s\n", pk.TopicName, string(pk.Payload))
	return pk, nil
}

func main() {
	// 1. Intercept execution logic if the dynamic self-installation flag is present
	installFlag := flag.Bool("install", false, "Install and configure the systemd service matching the current environment user context")
	flag.Parse()

	if *installFlag {
		err := installService()
		if err != nil {
			log.Fatalf("Installation failed: %v", err)
		}
		os.Exit(0)
	}
	// NEW: Verify service environment context if launched manually without arguments
	// Track whether we are running in an interactive manual terminal session
	useCurrentWd := false

	// Verify service environment context if launched manually without arguments
	if len(os.Args) == 1 {
		servicePath := "/etc/systemd/system/vessel-inventory.service"
		if _, err := os.Stat(servicePath); os.IsNotExist(err) {
			fmt.Println("⚠️  Notice: This server is not currently registered as a systemd background service.")
			fmt.Print("Would you like to run it directly in this terminal session? (y/N): ")

			var response string
			fmt.Scanln(&response)
			response = strings.ToLower(strings.TrimSpace(response))

			if response != "y" && response != "yes" {
				fmt.Println("\n--- Help Menu ---")
				flag.Usage()
				os.Exit(0)
			}

			// User opted to continue manually, target the local terminal workspace
			useCurrentWd = true
			fmt.Println("🚀 Launching local server session using current working directory...")
		}
	}

	// 2. Initialize Mochi MQTT Broker with Inline Client capabilities activated
	mqttServer := mqtt.New(&mqtt.Options{
		InlineClient: true,
	})

	tcpListener := listeners.NewTCP(listeners.Config{
		ID:      "vessel-tcp-listener",
		Address: "0.0.0.0:1883",
	})
	if err := mqttServer.AddListener(tcpListener); err != nil {
		log.Fatalf("Failed to add MQTT TCP listener: %v", err)
	}

	err := mqttServer.AddHook(new(ConsoleLoggerHook), nil)
	if err != nil {
		log.Fatalf("Failed to attach MQTT logger hook: %v", err)
	}

	wsListener := listeners.NewWebsocket(listeners.Config{
		ID:      "vessel-websocket-listener",
		Address: "0.0.0.0:1884",
	})
	if err := mqttServer.AddListener(wsListener); err != nil {
		log.Fatalf("Failed to add MQTT WebSocket listener: %v", err)
	}

	go func() {
		fmt.Println("Embedded MQTT Broker starting on 0.0.0.0:1883 & :1884...")
		if err := mqttServer.Serve(); err != nil {
			log.Fatalf("MQTT Broker crashed: %v", err)
		}
	}()
	// 3. Resolve Database File Path Coordinates Dynamically
	targetDataPath := defaultFilePath // Default fallback to relative filename string ("items.json")

	if useCurrentWd {
		workingDir, err := os.Getwd()
		if err == nil {
			targetDataPath = filepath.Join(workingDir, defaultFilePath)
		} else {
			log.Printf("⚠️  Failed to resolve working directory path: %v. Falling back to default routing.", err)
		}
	} else {
		// When running under systemd or with args, tie the dataset path directly to the binary's folder
		binaryPath, err := filepath.Abs(os.Args[0])
		if err == nil {
			targetDataPath = filepath.Join(filepath.Dir(binaryPath), defaultFilePath)
		}
	}

	log.Printf("📂 Database storage mapped to target tracking anchor: %s\n", targetDataPath)
	// 4. Initialize Data Store with structural defaults
	db := &Database{
		filePath: targetDataPath,
		data: Inventory{
			RootUIDs: []string{"0A:00:00:00:00:00:01"}, // Default structured root fallback anchor
			LastUID:  "0A:00:00:00:00:00:01",
			Items:    []Item{},
		},
		mqtt: mqttServer,
	}

	if err := db.load(); err != nil {
		log.Fatalf("Error loading JSON data: %v", err)
	}

	// 5. Register Explicit HTTP Routing Handlers
	http.HandleFunc("/items", db.handleItems)
	http.HandleFunc("/items/", db.handleIndividual)
	http.HandleFunc("/scan", db.handlePhoneScan)

	http.HandleFunc("/index.html", handleDashboardFile)
	http.HandleFunc("/scanner.html", handleScannerFile)

	// 6. Start Server on all interfaces (0.0.0.0)
	fmt.Println("Vessel Inventory Node online!")
	// Fetch all network interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Fatalf("Failed to get interfaces: %v", err)
	}

	fmt.Println("Available Network URLs:")
	for _, iface := range interfaces {
		// Skip interfaces that are down or are loopback (localhost)
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// Get addresses for this interface
		addrs, err := iface.Addrs()
		if err != nil {
			continue // Skip if we can't read addresses
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// Filter for IPv4 addresses only (to keep it clean for users)
			if ip == nil || ip.To4() == nil {
				continue
			}

			// Print the working URLs for your local network
			fmt.Printf(" -> [%s] http://%s:8080/index.html\n", iface.Name, ip)
			fmt.Printf(" -> [%s] http://%s:8080/scanner.html\n", iface.Name, ip)
		}
	}

	if err := http.ListenAndServe("0.0.0.0:8080", nil); err != nil {
		log.Fatalf("HTTP Server crashed: %v", err)
	}
}

// --- SYSTEMD INLINE INSTALLATION LOGIC ---

func installService() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installation requires root privileges; re-run with 'sudo'")
	}

	// Evaluate calling system user environment context (fallbacks handle clean mapping)
	targetUser := os.Getenv("SUDO_USER")
	if targetUser == "" {
		targetUser = os.Getenv("USER")
	}
	if targetUser == "" {
		targetUser = "root"
	}

	// Track dynamic execution coordinates
	binaryPath, err := filepath.Abs(os.Args[0])
	if err != nil {
		return fmt.Errorf("failed to determine binary path: %w", err)
	}
	workingDir := filepath.Dir(binaryPath)

	// Format systemd service dynamically utilizing runtime environmental parameters
	serviceConfig := fmt.Sprintf(`[Unit]
Description=SV Frog & Puffin Inventory Matrix Service
After=network.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
ExecStart=%s
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, targetUser, workingDir, binaryPath)

	servicePath := "/etc/systemd/system/vessel-inventory.service"
	err = os.WriteFile(servicePath, []byte(serviceConfig), 0644)
	if err != nil {
		return fmt.Errorf("failed to write service file configuration: %w", err)
	}
	fmt.Printf("✓ Written systemd unit configuration for host user: %s\n", targetUser)

	// Route service parameters via host system commands
	commands := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "vessel-inventory"},
		{"systemctl", "start", "vessel-inventory"},
	}

	for _, cmd := range commands {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed executing %s pipeline sequence: %s (%w)", cmd[0], string(out), err)
		}
	}

	fmt.Println("✓ Vessel Inventory successfully installed and running as a background service!")
	return nil
}

// --- STANDARD HANDLER FALLBACKS ---

func handleDashboardFile(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(indexHTML)
}

func handleScannerFile(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/scanner.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(scannerHTML)
}

func (db *Database) handlePhoneScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uid := r.URL.Query().Get("uid")
	if uid == "" {
		http.Error(w, "Missing 'uid' query parameter", http.StatusBadRequest)
		return
	}

	err := db.mqtt.Publish("vessel/scan", []byte(uid), false, 0)
	if err != nil {
		log.Printf("MQTT Publish Error: %v\n", err)
		http.Error(w, "Internal Broker error passing data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"success","message":"published to broker"}`))
}

func (db *Database) searchItems(w http.ResponseWriter, r *http.Request) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	query := r.URL.Query()
	var filtered []Item

	for _, item := range db.data.Items {
		if !item.Active {
			continue
		}

		match := true

		if uid := query.Get("uid"); uid != "" && item.UID != uid {
			match = false
		}
		if parent := query.Get("parent"); parent != "" && item.Parent != parent {
			match = false
		}
		if label := query.Get("label"); label != "" && item.Label != label {
			match = false
		}
		if qtyStr := query.Get("qty"); qtyStr != "" {
			if qty, err := strconv.Atoi(qtyStr); err != nil || item.Qty != qty {
				match = false
			}
		}
		if title := query.Get("title"); title != "" {
			if !strings.Contains(strings.ToLower(item.Title), strings.ToLower(title)) {
				match = false
			}
		}
		if desc := query.Get("description"); desc != "" {
			if !strings.Contains(strings.ToLower(item.Description), strings.ToLower(desc)) {
				match = false
			}
		}

		if match {
			filtered = append(filtered, item)
		}
	}

	json.NewEncoder(w).Encode(Inventory{
		RootUIDs: db.data.RootUIDs,
		LastUID:  db.data.LastUID,
		Items:    filtered,
	})
}

func (db *Database) getItemByUID(w http.ResponseWriter, uid string) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	for _, item := range db.data.Items {
		if item.UID == uid {
			if !item.Active {
				break
			}
			json.NewEncoder(w).Encode(item)
			return
		}
	}
	http.Error(w, "Item not found or is inactive", http.StatusNotFound)
}

func (db *Database) upsertItem(w http.ResponseWriter, r *http.Request) {
	var incoming Item
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil || incoming.UID == "" {
		http.Error(w, "Invalid payload. 'uid' is required.", http.StatusBadRequest)
		return
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if strings.HasPrefix(incoming.UID, "0A:") {
		incomingNorm := strings.ReplaceAll(incoming.UID, ":", "")
		lastNorm := strings.ReplaceAll(db.data.LastUID, ":", "")

		if incomingNorm > lastNorm {
			db.data.LastUID = incoming.UID
		}
	}

	foundIndex := -1
	for i, item := range db.data.Items {
		if item.UID == incoming.UID {
			foundIndex = i
			break
		}
	}

	if foundIndex != -1 {
		db.data.Items[foundIndex] = incoming
		w.WriteHeader(http.StatusOK)
	} else {
		db.data.Items = append(db.data.Items, incoming)
		w.WriteHeader(http.StatusCreated)
	}

	if err := db.save(); err != nil {
		http.Error(w, "Failed to persist data to disk", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(incoming)
}

func (db *Database) deleteItem(w http.ResponseWriter, uid string) {
	db.mu.Lock()
	defer db.mu.Unlock()

	foundIndex := -1
	for i, item := range db.data.Items {
		if item.UID == uid && item.Active {
			foundIndex = i
			break
		}
	}

	if foundIndex == -1 {
		http.Error(w, "Item not found or already inactive", http.StatusNotFound)
		return
	}

	db.data.Items[foundIndex].Active = false
	log.Printf("[Soft Delete] Item UID %s deactivated.\n", uid)

	if err := db.save(); err != nil {
		http.Error(w, "Failed to persist soft deletion to disk", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (db *Database) handleItems(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		db.searchItems(w, r)
	case http.MethodPost, http.MethodPut:
		db.upsertItem(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (db *Database) handleIndividual(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 || parts[2] == "" {
		http.Error(w, "Missing UID in path", http.StatusBadRequest)
		return
	}
	uid := parts[2]

	switch r.Method {
	case http.MethodGet:
		db.getItemByUID(w, uid)
	case http.MethodDelete:
		db.deleteItem(w, uid)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (db *Database) load() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if _, err := os.Stat(db.filePath); os.IsNotExist(err) {
		return nil
	}

	bytes, err := os.ReadFile(db.filePath)
	if err != nil {
		return err
	}

	return json.Unmarshal(bytes, &db.data)
}

func (db *Database) save() error {
	bytes, err := json.MarshalIndent(db.data, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(db.filePath, bytes, 0644)
}
