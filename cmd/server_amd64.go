package cmd

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/asticode/go-astilectron"
	bootstrap "github.com/asticode/go-astilectron-bootstrap"
	"github.com/asticode/go-astilog"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/nikhilsaraf/go-tools/multithreading"
	"github.com/rs/cors"
	"github.com/spf13/cobra"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/kelp/gui"
	"github.com/stellar/kelp/gui/backend"
	"github.com/stellar/kelp/support/kelpos"
	"github.com/stellar/kelp/support/logger"
	"github.com/stellar/kelp/support/networking"
	"github.com/stellar/kelp/support/prefs"
	"github.com/stellar/kelp/support/sdk"
)

const kelpPrefsDirectory = ".kelp"
const kelpAssetsPath = "/assets"
const logsDir = "/logs"
const vendorDirectory = "/vendor"
const trayIconName = "kelp-icon@1-8x.png"
const kelpCcxtPath = "/ccxt"
const ccxtDownloadBaseURL = "https://github.com/stellar/kelp/releases/download/ccxt-rest_v0.0.4"
const ccxtBinaryName = "ccxt-rest"
const ccxtWaitSeconds = 60
const versionPlaceholder = "VERSION_PLACEHOLDER"
const stringPlaceholder = "PLACEHOLDER_URL"
const redirectPlaceholder = "REDIRECT_URL"
const pingPlaceholder = "PING_URL"
const sleepNumSecondsBeforeReadyString = 1
const readyPlaceholder = "READY_STRING"
const readyStringIndicator = "Serving frontend and API server on HTTP port"

type serverInputs struct {
	port              *uint16
	dev               *bool
	devAPIPort        *uint16
	horizonTestnetURI *string
	horizonPubnetURI  *string
	noHeaders         *bool
	verbose           *bool
	noElectron        *bool
}

func init() {
	hasUICapability = true

	options := serverInputs{}
	options.port = serverCmd.Flags().Uint16P("port", "p", 8000, "port on which to serve")
	options.dev = serverCmd.Flags().Bool("dev", false, "run in dev mode for hot-reloading of JS code")
	options.devAPIPort = serverCmd.Flags().Uint16("dev-api-port", 8001, "port on which to run API server when in dev mode")
	options.horizonTestnetURI = serverCmd.Flags().String("horizon-testnet-uri", "https://horizon-testnet.stellar.org", "URI to use for the horizon instance connected to the Stellar Test Network (must contain the word 'test')")
	options.horizonPubnetURI = serverCmd.Flags().String("horizon-pubnet-uri", "https://horizon.stellar.org", "URI to use for the horizon instance connected to the Stellar Public Network (must not contain the word 'test')")
	options.noHeaders = serverCmd.Flags().Bool("no-headers", false, "do not set X-App-Name and X-App-Version headers on requests to horizon")
	options.verbose = serverCmd.Flags().BoolP("verbose", "v", false, "enable verbose log lines typically used for debugging")
	options.noElectron = serverCmd.Flags().Bool("no-electron", false, "open in browser instead of using electron")

	serverCmd.Run = func(ccmd *cobra.Command, args []string) {
		binDirectory, e := getBinaryDirectory()
		if e != nil {
			panic(errors.Wrap(e, "could not get binary directory"))
		}
		log.Printf("binDirectory: %s", binDirectory)

		isLocalMode := env == envDev
		isLocalDevMode := isLocalMode && *options.dev
		kos := kelpos.GetKelpOS()

		logFilepath := ""
		if !isLocalDevMode {
			l := logger.MakeBasicLogger()
			t := time.Now().Format("20060102T150405MST")
			logFilename := fmt.Sprintf("kelp-ui_%s.log", t)

			logsDirPath := filepath.Join(binDirectory, kelpPrefsDirectory, logsDir)
			log.Printf("making logsDirPath: %s ...", logsDirPath)
			e = kos.Mkdir(logsDirPath)
			if e != nil {
				panic(errors.Wrap(e, "could not make directories for logsDirPath: "+logsDirPath))
			}

			logFilepath = filepath.Join(logsDirPath, logFilename)
			setLogFile(l, logFilepath)

			if *options.verbose {
				astilog.SetDefaultLogger()
			}
		}

		// create a latch to trigger the browser opening once the backend server is loaded
		openBrowserWg := &sync.WaitGroup{}
		openBrowserWg.Add(1)
		if !isLocalDevMode {
			htmlContent := tailFileHTML
			if runtime.GOOS == "windows" {
				htmlContent = windowsInitialFile
			}

			appURL := fmt.Sprintf("http://localhost:%d", *options.port)
			pingURL := fmt.Sprintf("http://localhost:%d/ping", *options.port)
			// write out tail.html after setting the file to be tailed
			tailFileCompiled1 := strings.Replace(htmlContent, stringPlaceholder, logFilepath, -1)
			tailFileCompiled2 := strings.Replace(tailFileCompiled1, redirectPlaceholder, appURL, -1)
			tailFileCompiled3 := strings.Replace(tailFileCompiled2, readyPlaceholder, readyStringIndicator, -1)
			version := strings.TrimSpace(fmt.Sprintf("%s (%s)", guiVersion, version))
			tailFileCompiled4 := strings.Replace(tailFileCompiled3, versionPlaceholder, version, -1)
			tailFileCompiled5 := strings.Replace(tailFileCompiled4, pingPlaceholder, pingURL, -1)
			tailFilepath := filepath.Join(binDirectory, kelpPrefsDirectory, "tail.html")
			fileContents := []byte(tailFileCompiled5)
			e := ioutil.WriteFile(tailFilepath, fileContents, 0644)
			if e != nil {
				panic(fmt.Sprintf("could not write tailfile to path '%s': %s", tailFilepath, e))
			}

			// kick off the desktop window for UI feedback to the user
			// local mode (non --dev) and release binary should open browser (since --dev already opens browser via yarn and returns)
			trayIconPath, e := writeTrayIcon(kos)
			if e != nil {
				log.Fatal(errors.Wrap(e, "could not write tray icon"))
			}
			go func() {
				if *options.noElectron {
					openBrowser(kos, appURL, openBrowserWg)
				} else {
					openElectron(trayIconPath, tailFilepath)
				}
			}()
		}

		log.Printf("Starting Kelp GUI Server, gui=%s, cli=%s [%s]\n", guiVersion, version, gitHash)

		checkInitRootFlags()
		if !strings.Contains(*options.horizonTestnetURI, "test") {
			panic("'horizon-testnet-uri' argument must contain the word 'test'")
		}
		if strings.Contains(*options.horizonPubnetURI, "test") {
			panic("'horizon-pubnet-uri' argument must not contain the word 'test'")
		}

		horizonTestnetURI := strings.TrimSuffix(*options.horizonTestnetURI, "/")
		horizonPubnetURI := strings.TrimSuffix(*options.horizonPubnetURI, "/")
		log.Printf("using horizonTestnetURI: %s\n", horizonTestnetURI)
		log.Printf("using horizonPubnetURI: %s\n", horizonPubnetURI)

		if *rootCcxtRestURL == "" {
			*rootCcxtRestURL = "http://localhost:3000"
			e := sdk.SetBaseURL(*rootCcxtRestURL)
			if e != nil {
				panic(fmt.Errorf("unable to set CCXT-rest URL to '%s': %s", *rootCcxtRestURL, e))
			}
		}
		log.Printf("using ccxtRestUrl: %s\n", *rootCcxtRestURL)
		apiTestNet := &horizonclient.Client{
			HorizonURL: horizonTestnetURI,
			HTTP:       http.DefaultClient,
		}
		apiPubNet := &horizonclient.Client{
			HorizonURL: horizonPubnetURI,
			HTTP:       http.DefaultClient,
		}
		if !*options.noHeaders {
			apiTestNet.AppName = "kelp-ui"
			apiTestNet.AppVersion = version
			apiPubNet.AppName = "kelp-ui"
			apiPubNet.AppVersion = version

			p := prefs.Make(prefsFilename)
			if p.FirstTime() {
				log.Printf("Kelp sets the `X-App-Name` and `X-App-Version` headers on requests made to Horizon. These headers help us track overall Kelp usage, so that we can learn about general usage patterns and adapt Kelp to be more useful in the future. These can be turned off using the `--no-headers` flag. See `kelp trade --help` for more information.\n")
				e := p.SetNotFirstTime()
				if e != nil {
					log.Println("")
					log.Printf("unable to create preferences file: %s", e)
					// we can still proceed with this error
				}
			}
		}

		if isLocalDevMode {
			log.Printf("not checking ccxt in local dev mode")
		} else {
			// we need to check twice because sometimes the ccxt process lingers between runs so we can get a false positive on the first check
			e := checkIsCcxtUpTwice(*rootCcxtRestURL)
			ccxtRunning := e == nil
			log.Printf("checked if CCXT is already running, ccxtRunning = %v", ccxtRunning)

			if !ccxtRunning {
				// start ccxt before we make API server (which loads exchange list)
				ccxtGoos := runtime.GOOS
				if ccxtGoos == "windows" {
					ccxtGoos = "linux"
				}
				ccxtFilenameNoExt := fmt.Sprintf("ccxt-rest_%s-x64", ccxtGoos)
				ccxtDirPath, e := downloadCcxtBinary(kos, ccxtFilenameNoExt)
				if e != nil {
					panic(e)
				}

				e = runCcxtBinary(kos, ccxtDirPath, ccxtFilenameNoExt)
				if e != nil {
					panic(e)
				}
			}
		}

		s, e := backend.MakeAPIServer(kos, *options.horizonTestnetURI, apiTestNet, *options.horizonPubnetURI, apiPubNet, *rootCcxtRestURL, *options.noHeaders, quit)
		if e != nil {
			panic(e)
		}

		if isLocalDevMode {
			checkHomeDir()
			// the frontend app checks the REACT_APP_API_PORT variable to be set when serving
			os.Setenv("REACT_APP_API_PORT", fmt.Sprintf("%d", *options.devAPIPort))
			go runAPIServerDevBlocking(s, *options.port, *options.devAPIPort)
			runWithYarn(kos, options)

			log.Printf("should not have reached here after running yarn")
			return
		}

		options.devAPIPort = nil
		// the frontend app checks the REACT_APP_API_PORT variable to be set when serving
		os.Setenv("REACT_APP_API_PORT", fmt.Sprintf("%d", *options.port))

		if isLocalMode {
			checkHomeDir()
			generateStaticFiles(kos)
		}

		r := chi.NewRouter()
		setMiddleware(r)
		backend.SetRoutes(r, s)
		// gui.FS is automatically compiled based on whether this is a local or deployment build
		gui.FileServer(r, "/", gui.FS)

		portString := fmt.Sprintf(":%d", *options.port)
		log.Printf("starting server on port %d\n", *options.port)

		threadTracker := multithreading.MakeThreadTracker()
		e = threadTracker.TriggerGoroutine(func(inputs []interface{}) {
			if isLocalMode {
				e1 := http.ListenAndServe(portString, r)
				if e1 != nil {
					log.Fatal(e1)
				}
			} else {
				_ = http.ListenAndServe(portString, r)
			}
		}, nil)
		if e != nil {
			log.Fatal(e)
		}

		log.Printf("sleeping for %d seconds before showing the ready string indicator...\n", sleepNumSecondsBeforeReadyString)
		time.Sleep(sleepNumSecondsBeforeReadyString * time.Second)

		log.Printf("%s: %d\n", readyStringIndicator, *options.port)
		openBrowserWg.Done()
		threadTracker.Wait()

		log.Printf("should not have reached here after starting the backend server")
	}
}

func checkIsCcxtUpTwice(ccxtURL string) error {
	e := isCcxtUp(ccxtURL)
	if e != nil {
		return fmt.Errorf("ccxt-rest was not running on first check: %s", e)
	}

	// tiny pause before second check
	time.Sleep(100 * time.Millisecond)
	e = isCcxtUp(ccxtURL)
	if e != nil {
		return fmt.Errorf("ccxt-rest was not running on second check: %s", e)
	}

	// return nil for no error when it is running
	return nil
}

func setMiddleware(r *chi.Mux) {
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
}

func downloadCcxtBinary(kos *kelpos.KelpOS, filenameNoExt string) (string, error) {
	binDirectory, e := getBinaryDirectory()
	if e != nil {
		return "", errors.Wrap(e, "could not get binary directory")
	}
	log.Printf("binDirectory: %s", binDirectory)

	ccxtDirPath := filepath.Join(binDirectory, kelpPrefsDirectory, kelpCcxtPath)
	log.Printf("making ccxtDirPath: %s ...", ccxtDirPath)
	e = kos.Mkdir(ccxtDirPath)
	if e != nil {
		return "", errors.Wrap(e, "could not make directories for ccxtDirPath: "+ccxtDirPath)
	}

	filenameWithExt := fmt.Sprintf("%s.zip", filenameNoExt)
	ccxtZipDownloadPath := filepath.Join(ccxtDirPath, filenameWithExt)
	if _, e := os.Stat(ccxtZipDownloadPath); !os.IsNotExist(e) {
		return ccxtDirPath, nil
	}

	downloadURL := fmt.Sprintf("%s/%s", ccxtDownloadBaseURL, filenameWithExt)
	log.Printf("download ccxt from %s to location: %s", downloadURL, ccxtZipDownloadPath)
	networking.DownloadFile(downloadURL, ccxtZipDownloadPath)
	unzipCcxtFile(kos, ccxtDirPath, filenameNoExt, binDirectory)

	return ccxtDirPath, nil
}

func unzipCcxtFile(kos *kelpos.KelpOS, ccxtDir string, filenameNoExt string, binDirectory string) {
	zipFilename := filenameNoExt + ".zip"
	log.Printf("unzipping file %s ... ", zipFilename)
	zipCmd := fmt.Sprintf("cd %s && unzip %s && cd %s", ccxtDir, zipFilename, binDirectory)
	_, e := kos.Blocking("zip", zipCmd)
	if e != nil {
		log.Fatal(errors.Wrap(e, fmt.Sprintf("unable to unzip file %s in directory %s", zipFilename, ccxtDir)))
	}
	log.Printf("done\n")
}

func runCcxtBinary(kos *kelpos.KelpOS, ccxtDirPath string, ccxtFilenameNoExt string) error {
	ccxtBinPath := filepath.Join(ccxtDirPath, ccxtFilenameNoExt, ccxtBinaryName)
	if _, e := os.Stat(ccxtBinPath); os.IsNotExist(e) {
		return fmt.Errorf("path to ccxt binary (%s) does not exist", ccxtBinPath)
	}

	log.Printf("running binary %s", ccxtBinPath)
	// TODO CCXT should be run at the port specified by rootCcxtRestURL, currently it will default to port 3000 even if the config file specifies otherwise
	_, e := kos.Background("ccxt-rest", ccxtBinPath)
	if e != nil {
		log.Fatal(errors.Wrap(e, fmt.Sprintf("unable to run ccxt file %s", ccxtBinPath)))
	}

	log.Printf("waiting up to %d seconds for ccxt-rest to start up ...", ccxtWaitSeconds)
	for i := 0; i < ccxtWaitSeconds; i++ {
		e := isCcxtUp(*rootCcxtRestURL)
		ccxtRunning := e == nil

		if ccxtRunning {
			log.Printf("done, waited for ~%d seconds before CCXT was running\n", i)
			return nil
		}

		// wait
		log.Printf("ccxt is not up, sleeping for 1 second (waited so far = %d seconds)\n", i)
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("waited for %d seconds but CCXT was still not running at URL %s", ccxtWaitSeconds, *rootCcxtRestURL)
}

func runAPIServerDevBlocking(s *backend.APIServer, frontendPort uint16, devAPIPort uint16) {
	r := chi.NewRouter()
	// Add CORS middleware around every request since both ports are different when running server in dev mode
	r.Use(cors.New(cors.Options{
		AllowedOrigins: []string{fmt.Sprintf("http://localhost:%d", frontendPort)},
	}).Handler)

	setMiddleware(r)
	backend.SetRoutes(r, s)
	portString := fmt.Sprintf(":%d", devAPIPort)
	log.Printf("Serving API server on HTTP port: %d\n", devAPIPort)
	e := http.ListenAndServe(portString, r)
	log.Fatal(e)
}

func checkHomeDir() {
	op, e := exec.Command("pwd").Output()
	if e != nil {
		panic(e)
	}
	result := strings.TrimSpace(string(op))

	if !strings.HasSuffix(result, "/kelp") {
		log.Fatalf("need to invoke the '%s' command while in the root 'kelp' directory\n", serverCmd.Use)
	}
}

func runWithYarn(kos *kelpos.KelpOS, options serverInputs) {
	// yarn requires the PORT variable to be set when serving
	os.Setenv("PORT", fmt.Sprintf("%d", *options.port))

	log.Printf("Serving frontend via yarn on HTTP port: %d\n", *options.port)
	e := kos.StreamOutput(exec.Command("yarn", "--cwd", "gui/web", "start"))
	if e != nil {
		panic(e)
	}
}

func generateStaticFiles(kos *kelpos.KelpOS) {
	log.Printf("generating contents of gui/web/build ...\n")

	e := kos.StreamOutput(exec.Command("yarn", "--cwd", "gui/web", "build"))
	if e != nil {
		panic(e)
	}

	log.Printf("... finished generating contents of gui/web/build\n")
	log.Println()
}

func writeTrayIcon(kos *kelpos.KelpOS) (string, error) {
	binDirectory, e := getBinaryDirectory()
	if e != nil {
		return "", errors.Wrap(e, "could not get binary directory")
	}
	log.Printf("binDirectory: %s", binDirectory)
	assetsDirPath := filepath.Join(binDirectory, kelpPrefsDirectory, kelpAssetsPath)
	log.Printf("assetsDirPath: %s", assetsDirPath)
	trayIconPath := filepath.Join(assetsDirPath, trayIconName)
	log.Printf("trayIconPath: %s", trayIconPath)
	if _, e := os.Stat(trayIconPath); !os.IsNotExist(e) {
		// file exists, don't write again
		return trayIconPath, nil
	}

	trayIconBytes, e := resourcesKelpIcon18xPngBytes()
	if e != nil {
		return "", errors.Wrap(e, "could not fetch tray icon image bytes")
	}

	img, _, e := image.Decode(bytes.NewReader(trayIconBytes))
	if e != nil {
		return "", errors.Wrap(e, "could not decode bytes as image data")
	}

	// create dir if not exists
	if _, e := os.Stat(assetsDirPath); os.IsNotExist(e) {
		log.Printf("making assetsDirPath: %s ...", assetsDirPath)
		e = kos.Mkdir(assetsDirPath)
		if e != nil {
			return "", errors.Wrap(e, "could not make directories for assetsDirPath: "+assetsDirPath)
		}
		log.Printf("... made assetsDirPath (%s)", assetsDirPath)
	}

	trayIconFile, e := os.Create(trayIconPath)
	if e != nil {
		return "", errors.Wrap(e, "could not create tray icon file")
	}
	defer trayIconFile.Close()

	e = png.Encode(trayIconFile, img)
	if e != nil {
		return "", errors.Wrap(e, "could not write png encoded icon")
	}

	return trayIconPath, nil
}

func getBinaryDirectory() (string, error) {
	return filepath.Abs(filepath.Dir(os.Args[0]))
}

func openBrowser(kos *kelpos.KelpOS, url string, openBrowserWg *sync.WaitGroup) {
	log.Printf("opening URL in native browser: %s", url)

	var browserCmd string
	if runtime.GOOS == "linux" {
		browserCmd = fmt.Sprintf("xdg-open %s", url)
	} else if runtime.GOOS == "darwin" {
		browserCmd = fmt.Sprintf("open %s", url)
	} else if runtime.GOOS == "windows" {
		browserCmd = fmt.Sprintf("start %s", url)
	} else {
		log.Fatalf("unable to open url '%s' in browser because runtime.GOOS was unrecognized: %s", url, runtime.GOOS)
	}

	openBrowserWg.Wait()
	_, e := kos.Blocking("browser", browserCmd)
	if e != nil {
		log.Fatal(e)
	}
}

func openElectron(trayIconPath string, url string) {
	log.Printf("opening URL in electron: %s", url)
	e := bootstrap.Run(bootstrap.Options{
		AstilectronOptions: astilectron.Options{
			AppName:            "Kelp",
			AppIconDefaultPath: "resources/kelp-icon@2x.png",
			AcceptTCPTimeout:   time.Minute * 2,
		},
		Debug: false,
		Windows: []*bootstrap.Window{&bootstrap.Window{
			Homepage: url,
			Options: &astilectron.WindowOptions{
				Center:   astilectron.PtrBool(true),
				Width:    astilectron.PtrInt(1280),
				Height:   astilectron.PtrInt(960),
				Closable: astilectron.PtrBool(false),
			},
		}},
		TrayOptions: &astilectron.TrayOptions{
			Image: astilectron.PtrStr(trayIconPath),
		},
		TrayMenuOptions: []*astilectron.MenuItemOptions{
			&astilectron.MenuItemOptions{
				Label:   astilectron.PtrStr("Quit"),
				Visible: astilectron.PtrBool(true),
				Enabled: astilectron.PtrBool(true),
				OnClick: astilectron.Listener(func(e astilectron.Event) (deleteListener bool) {
					quit()
					return false
				}),
			},
		},
	})
	if e != nil {
		log.Fatal(e)
	}

	quit()
}

func quit() {
	log.Printf("quitting...")
	os.Exit(0)
}

const windowsInitialFile = `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN" "http://www.w3.org/TR/html4/loose.dtd">
<html>
	<head>
		<title>Kelp GUI VERSION_PLACEHOLDER</title>
		<script type="text/javascript">
			if (typeof XMLHttpRequest == "undefined") {
				// this is only for really ancient browsers
				XMLHttpRequest = function () {
					try { return new ActiveXObject("Msxml2.xmlHttp.6.0"); }
					catch (e1) { }
					try { return new ActiveXObject("Msxml2.xmlHttp.3.0"); }
					catch (e2) { }
					try { return new ActiveXObject("Msxml2.xmlHttp"); }
					catch (e3) { }
					throw new Error("This browser does not support xmlHttpRequest.");
				};
			}

			var pingUrl = "PING_URL";
			var redirectUrl = "REDIRECT_URL";
			function checkServerOnline() {
				var ajax = new XMLHttpRequest();
				ajax.open("GET", pingUrl, true);
				ajax.onreadystatechange = function () {
					if ((ajax.readyState == 4) && (ajax.status == 200)) {
						window.location.href = redirectUrl;
					}
				}
				ajax.send(null);
			}
		</script>
	</head>
	<body onLoad='setInterval("checkServerOnline()", 500);' bgcolor="#0D0208" text="#00FF41">
		<div>
			Loading the backend for Kelp.<br />
			This will take a few minutes.<br />
			<br />
			You will be redirected automatically once loaded.<br />
			<br />
			Please be patient.<br />
		</div>
	</body>
</html>
`

const tailFileHTML = `<!-- taken from http://www.davejennifer.com/computerjunk/javascript/tail-dash-f.html with minor modifications -->
<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN" "http://www.w3.org/TR/html4/loose.dtd">
<html>
	<head>
		<title>Kelp GUI VERSION_PLACEHOLDER</title>

		<style>
			.button {
				background-color: #003B00; /* Dark Green */
				color: #00FF41;
				border: 2px solid #00FF41;
				padding: 15px 32px;
				text-align: center;
				text-decoration: none;
				font-size: 16px;
				cursor: pointer;
			}
		</style>
		<script type="text/javascript">
			var lastByte = 0;

			if (typeof XMLHttpRequest == "undefined") {
				// this is only for really ancient browsers
				XMLHttpRequest = function () {
					try { return new ActiveXObject("Msxml2.xmlHttp.6.0"); }
					catch (e1) { }
					try { return new ActiveXObject("Msxml2.xmlHttp.3.0"); }
					catch (e2) { }
					try { return new ActiveXObject("Msxml2.xmlHttp"); }
					catch (e3) { }
					throw new Error("This browser does not support xmlHttpRequest.");
				};
			}

			// Substitute the URL for your server log file here...
			//
			var url = "PLACEHOLDER_URL";

			var visible = false;
			function tailf() {
				var ajax = new XMLHttpRequest();
				ajax.open("POST", url, true);

				if (lastByte == 0) {
					// First request - get everything
				} else {
					//
					// All subsequent requests - add the Range header
					//
					ajax.setRequestHeader("Range", "bytes=" + parseInt(lastByte) + "-");
				}

				ajax.onreadystatechange = function () {
					if (ajax.readyState == 4) {
						if (ajax.status == 200) {
							// only the first request
							lastByte = parseInt(ajax.getResponseHeader("Content-length"));
							document.getElementById("thePlace").innerHTML = ajax.responseText;
							if (visible) {
								document.getElementById("theEnd").scrollIntoView();
							}
						} else if (ajax.status == 206) {
							lastByte += parseInt(ajax.getResponseHeader("Content-length"));
							document.getElementById("thePlace").innerHTML += ajax.responseText;
							if (visible) {
								document.getElementById("theEnd").scrollIntoView();
							}
						} else if (ajax.status == 416) {
							// no new data, so do nothing
						} else {
							//  Some error occurred - just display the status code and response
							alert("Ajax status: " + ajax.status + "\n" + ajax.getAllResponseHeaders());
						}
						
						if (ajax.status == 200 || ajax.status == 206) {
							if (ajax.responseText.includes("READY_STRING")) {
								var redirectURL = "REDIRECT_URL";
								document.getElementById("theEnd").innerHTML = "<br/><br/><b>redirecting to " + redirectURL + " ...</b><br/><br/>";
								document.getElementById("theEnd").scrollIntoView();
								// sleep for 2 seconds so the user sees that we are being redirected
								setTimeout(() => { window.location.href = redirectURL; }, 2000)
							}
						}
					}// ready state 4
				}//orsc function def

				ajax.send(null);

			}// function tailf
		</script>
	
		<script type="text/javascript">
			function onInit() {
				document.getElementById("overHood").style.visibility = "visible";
				document.getElementById("underHood").style.visibility = "hidden";
			}

			function liftHood() {
				document.getElementById("overHood").style.visibility = "hidden";
				document.getElementById("underHood").style.visibility = "visible";
				visible = true;
				document.getElementById("theEnd").scrollIntoView();
			}
		</script>
	</head>

	<body onLoad='onInit(); tailf(); setInterval("tailf()", 250);' bgcolor="#0D0208" text="#00FF41">
		<div>
			<div id="overHood">
				<center>
					<button class="button" onclick='liftHood();'>Show Me What's Under The Hood</button>
				</center>
			</div>
			<div id="underHood">
				<pre id="thePlace"/>
			</div>
			<div id="theEnd"/>
		</div>
	</body>
</html>
`
