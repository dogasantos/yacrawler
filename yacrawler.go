package main
// github.com/dogasantos/yacrawler
import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly"
)

var (
	startURL        string
	urlListFile     string
	verbose         bool
	includeQuery    bool
	matchString     string
	parallelism     int
	depth           int
	timeToCrawl     int
	extensionPolicy string
	formatOption    string
	simplifyOutput  bool
	matchJS         bool
	matchHTML       bool
	countPages      bool
	pageCounts      = make(map[string]int)
	mu              sync.Mutex
)

func init() {
	flag.StringVar(&startURL, "u", "", "Specify the URL to crawl")
	flag.StringVar(&urlListFile, "l", "", "Specify the list of URLs to crawl (one URL per line in a text file)")
	flag.BoolVar(&verbose, "v", false, "Print both internal and external references and include query strings in the output when using -f link")
	flag.BoolVar(&includeQuery, "q", false, "Include query strings in the output")
	flag.StringVar(&matchString, "ms", "", "Match string for filtering specific URLs")
	flag.IntVar(&parallelism, "t", 4, "Specify how many URLs should be crawled in parallel")
	flag.IntVar(&depth, "d", 1, "Specify the depth of the crawl")
	flag.IntVar(&timeToCrawl, "ttc", 10, "Specify the maximum time-to-crawl in seconds")
	flag.StringVar(&extensionPolicy, "e", "", "Specify crawling extension policy: rdn (root domain name) or all (all URLs)")
	flag.StringVar(&formatOption, "f", "link", "Specify the output format: link, fqdn, origin, domain")
	flag.BoolVar(&simplifyOutput, "s", false, "Simplify output by printing each attribute value only once per FQDN")
	flag.BoolVar(&matchJS, "js", false, "Fetch and match the string in JavaScript files")
	flag.BoolVar(&matchHTML, "html", false, "Fetch and match the string in HTML files")
	flag.BoolVar(&countPages, "count", false, "Count the number of internal pages the website contains")
}

func main() {
	flag.Parse()

	var urlsToCrawl []string
	if startURL != "" {
		urlsToCrawl = append(urlsToCrawl, startURL)
	}

	if urlListFile != "" {
		file, err := os.Open(urlListFile)
		if err != nil {
			log.Fatalf("Failed to open URL list file: %v", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			urlsToCrawl = append(urlsToCrawl, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Fatalf("Failed to read URL list file: %v", err)
		}
	}

	var wg sync.WaitGroup
	urlChan := make(chan string, parallelism)
	wg.Add(parallelism)

	for i := 0; i < parallelism; i++ {
		go func() {
			defer wg.Done()
			for url := range urlChan {
				crawl(url)
			}
		}()
	}

	for _, url := range urlsToCrawl {
		urlChan <- url
	}
	close(urlChan)
	wg.Wait()

	if countPages {
		mu.Lock()
		for origin, count := range pageCounts {
			fmt.Printf("%s:%d\n", origin, count)
		}
		mu.Unlock()
	}
}

func crawl(startURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeToCrawl)*time.Second)
	defer cancel()

	startFQDN := getFQDN(startURL)

	c := colly.NewCollector(
		colly.MaxDepth(depth),
		colly.Async(true),
		colly.AllowedDomains(startFQDN),
	)

	c.WithTransport(&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: 1,
	})

	c.SetRequestTimeout(time.Duration(timeToCrawl) * time.Second)

	visited := make(map[string]struct{})
	pageCount := 0

	c.OnHTML("a[href], img[src], link[href], script[src]", func(e *colly.HTMLElement) {
		attribute := ""
		value := ""

		if e.Attr("href") != "" {
			attribute = "href"
			value = e.Request.AbsoluteURL(e.Attr("href"))
		} else if e.Attr("src") != "" {
			attribute = "src"
			value = e.Request.AbsoluteURL(e.Attr("src"))
		}

		if value == "" {
			return
		}

		valueFQDN := getFQDN(value)
		if valueFQDN != startFQDN {
			return
		}

		if !includeQuery && !(verbose && formatOption == "link") {
			u, err := url.Parse(value)
			if err == nil {
				u.RawQuery = ""
				value = u.String()
			}
		}

		// Fetch and check content for JS and HTML files
		if matchString != "" {
			if matchJS && strings.HasSuffix(value, ".js") {
				go fetchAndCheckContent(value, matchString)
				return
			} else if matchHTML && strings.HasSuffix(value, ".html") {
				go fetchAndCheckContent(value, matchString)
				return
			}
		}

		var key string
		switch formatOption {
		case "link":
			key = value
			if verbose {
				key = valueWithQuery(e.Request.URL.String(), value)
			}
		case "fqdn":
			key = getFQDN(value)
		case "origin":
			key = getOrigin(value)
		case "domain":
			key = getDomain(value)
		}

		key = fmt.Sprintf("%s:%s", attribute, key)

		if simplifyOutput {
			if _, found := visited[key]; found {
				return
			}
			visited[key] = struct{}{}
		}

		if countPages && isInternal(e.Request.URL, value) {
			if _, found := visited[value]; !found {
				pageCount++
				visited[value] = struct{}{}
			}
		}

		if !countPages {
			var output string
			switch formatOption {
			case "link":
				output = fmt.Sprintf("%s:%s:%s", stripQuery(e.Request.URL.String()), attribute, value)
				if verbose {
					output = fmt.Sprintf("%s:%s:%s", stripQuery(e.Request.URL.String()), attribute, valueWithQuery(e.Request.URL.String(), value))
				}
			case "fqdn":
				output = fmt.Sprintf("%s:%s:%s", stripQuery(e.Request.URL.String()), attribute, getFQDN(value))
			case "origin":
				output = fmt.Sprintf("%s:%s:%s", stripQuery(e.Request.URL.String()), attribute, getOrigin(value))
			case "domain":
				output = fmt.Sprintf("%s:%s:%s", stripQuery(e.Request.URL.String()), attribute, getDomain(value))
			}

			if verbose || (!verbose && isExternal(e.Request.URL, value)) {
				fmt.Println(output)
			}
		}

		if depth > 1 && (verbose || (!verbose && isInternal(e.Request.URL, value))) {
			e.Request.Visit(value)
		}
	})

	c.OnScraped(func(r *colly.Response) {
		if countPages {
			mu.Lock()
			origin := getOrigin(r.Request.URL.String())
			pageCounts[origin] += pageCount
			mu.Unlock()
			cancel()
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		// Handle errors silently
		if countPages {
			mu.Lock()
			origin := getOrigin(r.Request.URL.String())
			pageCounts[origin] += pageCount
			mu.Unlock()
			cancel()
		}
	})

	go func() {
		c.Visit(startURL)
		c.Wait()
	}()

	select {
	case <-ctx.Done():
		if countPages {
			mu.Lock()
			origin := getOrigin(startURL)
			pageCounts[origin] += pageCount
			mu.Unlock()
		}
		cancel()
	}
}

func fetchAndCheckContent(fileURL, matchString string) {
	resp, err := http.Get(fileURL)
	if err != nil {
		log.Printf("Failed to fetch %s: %v", fileURL, err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read body of %s: %v", fileURL, err)
		return
	}

	if strings.Contains(string(body), matchString) {
		fmt.Println(fileURL)
	}
}

func getAllowedDomains(startURL string) []string {
	u, err := url.Parse(startURL)
	if err != nil {
		return nil
	}

	switch extensionPolicy {
	case "rdn":
		parts := strings.Split(u.Hostname(), ".")
		if len(parts) > 2 {
			parts = parts[len(parts)-2:]
		}
		return []string{strings.Join(parts, ".")}
	case "all":
		return nil
	default:
		return []string{u.Hostname()}
	}
}

func isExternal(base *url.URL, href string) bool {
	u, err := url.Parse(href)
	if err != nil {
		return false
	}
	return base.Hostname() != u.Hostname()
}

func isInternal(base *url.URL, href string) bool {
	u, err := url.Parse(href)
	if err != nil {
		return false
	}

	switch extensionPolicy {
	case "rdn":
		baseParts := strings.Split(base.Hostname(), ".")
		if len(baseParts) > 2 {
			baseParts = baseParts[len(baseParts)-2:]
		}
		baseDomain := strings.Join(baseParts, ".")

		hrefParts := strings.Split(u.Hostname(), ".")
		if len(hrefParts) > 2 {
			hrefParts = hrefParts[len(hrefParts)-2:]
		}
		hrefDomain := strings.Join(hrefParts, ".")

		return baseDomain == hrefDomain
	case "all":
		return true
	default:
		return base.Hostname() == u.Hostname()
	}
}

func stripQuery(input string) string {
	u, err := url.Parse(input)
	if err != nil {
		return input
	}
	u.RawQuery = ""
	return u.String()
}

func valueWithQuery(origin, value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return value
	}
	if u.RawQuery == "" {
		originURL, err := url.Parse(origin)
		if err == nil {
			u.RawQuery = originURL.RawQuery
		}
	}
	return u.String()
}

func getFQDN(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func getOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	origin := u.Scheme + "://" + u.Host
	if u.Port() != "" {
		origin += ":" + u.Port()
	}
	return origin
}

func getDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}
