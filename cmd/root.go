package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall" // Required for syscall.SIGTERM

	"kubetuunel/internal"

	"github.com/spf13/cobra"
)

var configPath string

// run is the main logic for the Cobra command.
func run(cmd *cobra.Command, args []string) {
	servicesToUse, _ := cmd.Flags().GetStringSlice("services")
	sitesToUse := args
	if len(sitesToUse) == 0 {
		for name := range internal.Cfg.Sites {
			sitesToUse = append(sitesToUse, name)
		}
		if len(sitesToUse) == 0 {
			log.Println("ℹ️ No sites specified and no sites found in the configuration to process.")
			return
		}
	}

	var wg sync.WaitGroup
	// allActiveStopFuncs will store all the stop functions for successfully established tunnels.
	var allActiveStopFuncs []func()
	var mu sync.Mutex // Mutex to protect concurrent appends to allActiveStopFuncs

	log.Printf("🔍 Attempting to forward services for sites: %v", sitesToUse)

	for _, siteName := range sitesToUse {
		// Ensure site configuration exists.
		site, ok := internal.Cfg.Sites[siteName]
		if !ok {
			log.Printf("⚠️ Site '%s' not found in config, skipping...", siteName)
			continue
		}

		// Determine services for the current site.
		// If --services flag is used, it applies to all specified sites.
		// If --services is not used, all services for the current site are forwarded.
		actualServices := servicesToUse
		if len(actualServices) == 0 { // No global services filter, so use all from this site
			if site.Services == nil {
				log.Printf("ℹ️ Site '%s' has no services defined, skipping...", siteName)
				continue
			}
			for name := range site.Services {
				actualServices = append(actualServices, name)
			}
			if len(actualServices) == 0 {
				log.Printf("ℹ️ No services to forward for site '%s'.", siteName)
				continue
			}
		}

		wg.Add(1)
		go func(currentSiteName string, servicesToForward []string) {
			defer wg.Done()
			log.Printf("🚀 Starting forwarding for site: %s, services: %v", currentSiteName, servicesToForward)

			// Call the updated internal.Forward function
			stopFuncsForSite, err := internal.Forward(currentSiteName, servicesToForward...)

			// Lock before modifying shared slice
			mu.Lock()
			if len(stopFuncsForSite) > 0 {
				allActiveStopFuncs = append(allActiveStopFuncs, stopFuncsForSite...)
				log.Printf("✅ Successfully established %d tunnel(s) for site %s.", len(stopFuncsForSite), currentSiteName)
			}
			mu.Unlock()

			if err != nil {
				// The error from internal.Forward might indicate partial success.
				// stopFuncsForSite could still have functions for tunnels that did start.
				log.Printf("❌ Error during forwarding for site %s: %v", currentSiteName, err)
			}

		}(siteName, actualServices)
	}

	wg.Wait()

	//// After all attempts, check if any tunnels are active.
	mu.Lock() // Lock to safely read len(allActiveStopFuncs)
	activeTunnelCount := len(allActiveStopFuncs)
	mu.Unlock()

	if activeTunnelCount == 0 {
		log.Println("🚫 No tunnels were successfully established. Exiting.")
		return // Or os.Exit(1) if it should be an error state
	}

	// print summary of active tunnels
	fmt.Printf("-----------------------------------------------------\n")
	fmt.Printf("📊 Tunnel Summary:\n")
	for siteName, site := range internal.Cfg.Sites {
		for serviceName, service := range site.Services {
			fmt.Printf("  - Site: %s, Service: %s (localhost:%d -> %s:%d)\n",
				siteName, serviceName, service.LocalPort, service.Endpoint, service.DefaultPort)
		}
	}
	fmt.Printf("-----------------------------------------------------\n")
	log.Printf("🎉 %d tunnel(s) are now active. Press Ctrl+C to stop and exit.", activeTunnelCount)

	// Set up channel to listen for OS interrupt signals.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// Block until a signal is received.
	<-signalChan

	log.Println("\n🚦 Received interrupt signal. Shutting down active tunnels...")

	// Call all collected stop functions.
	mu.Lock() // Lock for safe iteration, though no new appends should happen now.
	for i, stopFunc := range allActiveStopFuncs {
		if stopFunc != nil {
			log.Printf("🔌 Stopping tunnel %d/%d...", i+1, activeTunnelCount)
			stopFunc()
		}
	}
	mu.Unlock()

	log.Println("✅ All active tunnels have been shut down. Exiting.")
}

var rootCmd = &cobra.Command{
	Use:   "kubetunnel [site1 site2 ...] [--services svc1 svc2 ...]",
	Short: "Create tunnels to access Azure PaaS services (like Azure Database, Redis) through a proxy pod in AKS",
	Long: `Kubetunnel helps you access Azure PaaS services that are not directly accessible from your local machine.
It works by utilizing a proxy pod (usually running tools like 'socat') deployed in your AKS cluster
that has access to these Azure services.

This tool creates tunnels through the proxy pod to reach Azure managed services such as:
  - Azure Database for MySQL
  - Azure Cache for Redis
  - Other Azure PaaS services with private endpoints

Prerequisites:
  - An AKS cluster with a proxy pod that has access to the target Azure services
  - Proper network configuration allowing the proxy pod to reach the services

Specify sites to connect to as arguments. If no sites are given, it attempts
to connect to all sites defined in the configuration.
Use the --services flag to filter which services to forward for the specified sites.
If --services is not provided, all services for the selected sites will be forwarded.`,
	Run: run,
}

// loadConfig loads the configuration from the path specified by the --config flag.
// It's typically called by cobra.OnInitialize.
func loadConfig() {
	internal.Init(configPath)
}

// init is called when the package is imported.
// It sets up flags and initialization.
func init() {
	// PersistentFlags are global for the application.
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "./config/sites.yaml", "Path to config file (e.g., ./config/sites.yaml)")
	// Flags are local to this command.
	rootCmd.Flags().StringSlice("services", []string{}, "Comma-separated list of service names to forward (e.g., mysql,redis). If empty, all services for a site are forwarded.")

	// cobra.OnInitialize ensures loadConfig is called after flags are parsed
	// but before the command's Run function is executed.
	cobra.OnInitialize(loadConfig)
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
