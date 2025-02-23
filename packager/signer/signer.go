// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package signer

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/WICG/webpackage/go/signedexchange"
	"github.com/ampproject/amppackager/packager/accept"
	"github.com/ampproject/amppackager/packager/amp_cache_transform"
	"github.com/ampproject/amppackager/packager/certcache"
	"github.com/ampproject/amppackager/packager/mux"
	"github.com/ampproject/amppackager/packager/rtv"
	"github.com/ampproject/amppackager/packager/util"
	"github.com/ampproject/amppackager/transformer"
	rpb "github.com/ampproject/amppackager/transformer/request"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The user agent to send when issuing fetches. Should look like a mobile device.
const userAgent = "Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/41.0.2272.96 Mobile " +
	"Safari/537.36 (compatible; amppackager/0.0.0; +https://github.com/ampproject/amppackager)"

// Advised against, per
// https://tools.ietf.org/html/draft-yasskin-httpbis-origin-signed-exchanges-impl-00#section-4.1
// and blocked in http://crrev.com/c/958945.
var statefulResponseHeaders = map[string]bool{
	"Authentication-Control":    true,
	"Authentication-Info":       true,
	"Clear-Site-Data":           true,
	"Optional-WWW-Authenticate": true,
	"Proxy-Authenticate":        true,
	"Proxy-Authentication-Info": true,
	"Public-Key-Pins":           true,
	"Sec-WebSocket-Accept":      true,
	"Set-Cookie":                true,
	"Set-Cookie2":               true,
	"SetProfile":                true,
	"Strict-Transport-Security": true,
	"WWW-Authenticate":          true,
}

// The server generating a 304 response MUST generate any of the
// following header fields that would have been sent in a 200 (OK) response
// to the same request.
// https://tools.ietf.org/html/rfc7232#section-4.1
var statusNotModifiedHeaders = map[string]bool{
	"Cache-Control":    true,
	"Content-Location": true,
	"Date":             true,
	"ETag":             true,
	"Expires":          true,
	"Vary":             true,
}

// The current maximum is defined at:
// https://cs.chromium.org/chromium/src/content/browser/loader/merkle_integrity_source_stream.cc?l=18&rcl=591949795043a818e50aba8a539094c321a4220c
// The maximum is cheapest in terms of network usage, and probably CPU on both
// server and client. The memory usage difference is negligible.
const miRecordSize = 16 << 10

// Overrideable for testing.
var getTransformerRequest = func(r *rtv.RTVCache, s, u string) *rpb.Request {
	return &rpb.Request{Html: string(s), DocumentUrl: u, Rtv: r.GetRTV(), Css: r.GetCSS(),
		AllowedFormats: []rpb.Request_HtmlFormat{rpb.Request_AMP}}
}

// Roughly matches the protocol grammar
// (https://tools.ietf.org/html/rfc7230#section-6.7), which is defined in terms
// of token (https://tools.ietf.org/html/rfc7230#section-3.2.6). This differs
// in that it allows multiple slashes, as well as initial and terminal slashes.
var protocol = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9a-zA-Z/]+$")

// Gets all values of the named header, joined on comma.
func GetJoined(h http.Header, name string) string {
	if values, ok := h[http.CanonicalHeaderKey(name)]; ok {
		// See Note on https://tools.ietf.org/html/rfc7230#section-3.2.2.
		if http.CanonicalHeaderKey(name) == "Set-Cookie" && len(values) > 0 {
			return values[0]
		} else {
			return strings.Join(values, ", ")
		}
	} else {
		return ""
	}
}

type Signer struct {
	// TODO(twifkak): Support multiple certs. This will require generating
	// a signature for each one. Note that Chrome only supports 1 signature
	// at the moment.
	certHandler certcache.CertHandler
	// TODO(twifkak): Do we want to allow multiple keys?
	key                     crypto.PrivateKey
	client                  *http.Client
	urlSets                 []util.URLSet
	rtvCache                *rtv.RTVCache
	shouldPackage           func() error
	overrideBaseURL         *url.URL
	requireHeaders          bool
	forwardedRequestHeaders []string
	timeNow                 func() time.Time
}

func noRedirects(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

func New(certHandler certcache.CertHandler, key crypto.PrivateKey, urlSets []util.URLSet,
	rtvCache *rtv.RTVCache, shouldPackage func() error, overrideBaseURL *url.URL,
	requireHeaders bool, forwardedRequestHeaders []string, timeNow func() time.Time) (*Signer, error) {
	client := http.Client{
		CheckRedirect: noRedirects,
		// TODO(twifkak): Load-test and see if default transport settings are okay.
		Timeout: 60 * time.Second,
	}

	return &Signer{certHandler, key, &client, urlSets, rtvCache, shouldPackage, overrideBaseURL, requireHeaders, forwardedRequestHeaders, timeNow}, nil
}

func (this *Signer) fetchURL(fetch *url.URL, serveHTTPReq *http.Request) (*http.Request, *http.Response, *util.HTTPError) {
	ampURL := fetch.String()

	log.Printf("Fetching URL: %q\n", ampURL)
	req, err := http.NewRequest(http.MethodGet, ampURL, nil)
	if err != nil {
		return nil, nil, util.NewHTTPError(http.StatusInternalServerError, "Error building request: ", err)
	}
	req.Header.Set("User-Agent", userAgent)
	// copy forwardedRequestHeaders
	for _, header := range this.forwardedRequestHeaders {
		if http.CanonicalHeaderKey(header) == "Host" {
			req.Host = serveHTTPReq.Host
		} else if value := GetJoined(serveHTTPReq.Header, header); value != "" {
			req.Header.Set(header, value)
		}
	}
	// Golang's HTTP parser appears not to validate the protocol it parses
	// from the request line, so we do so here.
	if protocol.MatchString(serveHTTPReq.Proto) {
		// Set Via per https://tools.ietf.org/html/rfc7230#section-5.7.1.
		via := strings.TrimPrefix(serveHTTPReq.Proto, "HTTP/") + " " + "amppkg"
		if upstreamVia := GetJoined(req.Header, "Via"); upstreamVia != "" {
			via = upstreamVia + ", " + via
		}
		req.Header.Set("Via", via)
	}
	if quotedHost, err := util.QuotedString(serveHTTPReq.Host); err == nil {
		// TODO(twifkak): Extract host from upstream Forwarded header
		// and concatenate. (Do not include any other parameters, as
		// they may lead to over-signing.)
		req.Header.Set("Forwarded", `host=`+quotedHost)
		xfh := serveHTTPReq.Host
		if oldXFH := serveHTTPReq.Header.Get("X-Forwarded-Host"); oldXFH != "" {
			xfh = oldXFH + "," + xfh
		}
		req.Header.Set("X-Forwarded-Host", xfh)
	}
	// Set conditional headers that were included in ServeHTTP's Request.
	for header := range util.ConditionalRequestHeaders {
		if value := GetJoined(serveHTTPReq.Header, header); value != "" {
			req.Header.Set(header, value)
		}
	}
	resp, err := this.client.Do(req)
	if err != nil {
		return nil, nil, util.NewHTTPError(http.StatusBadGateway, "Error fetching: ", err)
	}
	util.RemoveHopByHopHeaders(resp.Header)
	return req, resp, nil
}

// Some Content-Security-Policy (CSP) configurations have the ability to break
// AMPHTML document functionality on the AMPHTML Cache if set on the document.
// This method parses the publisher's provided CSP and mutates it to ensure
// that the document is not broken on the AMP Cache.
//
// Specifically, the following CSP directives are passed through unmodified:
//  - base-uri
//  - block-all-mixed-content
//  - font-src
//  - form-action
//  - manifest-src
//  - referrer
//  - upgrade-insecure-requests
// And the following CSP directives are overridden to specific values:
//  - object-src
//  - report-uri
//  - script-src
//  - style-src
//  - default-src
// All other CSP directives (see https://w3c.github.io/webappsec-csp/) are
// stripped from the publisher provided CSP.
func MutateFetchedContentSecurityPolicy(fetched string) string {
	directiveTokens := strings.Split(fetched, ";")
	var newCsp strings.Builder
	for _, directiveToken := range directiveTokens {
		trimmed := strings.TrimSpace(directiveToken)
		// This differs from the spec slightly in that it allows U+000b vertical
		// tab in its definition of white space.
		directiveParts := strings.Fields(trimmed)
		if len(directiveParts) == 0 {
			continue
		}
		directiveName := strings.ToLower(directiveParts[0])
		switch directiveName {
		// Preserve certain directives. The rest are all removed or replaced.
		case "base-uri", "block-all-mixed-content", "font-src", "form-action",
			"manifest-src", "referrer", "upgrade-insecure-requests":
			newCsp.WriteString(trimmed)
			newCsp.WriteString(";")
		default:
		}
	}
	// Add missing directives or replace the ones that were removed in some cases
	// NOTE: After changing this string, please update the permalink in
	// docs/cache_requirements.md.
	newCsp.WriteString(
		"default-src * blob: data:;" +
			"report-uri https://csp.withgoogle.com/csp/amp;" +
			"script-src blob: https://cdn.ampproject.org/rtv/ " +
			"https://cdn.ampproject.org/v0.js " +
			"https://cdn.ampproject.org/v0/ " +
			"https://cdn.ampproject.org/lts/ " +
			"https://cdn.ampproject.org/viewer/;" +
			"style-src 'unsafe-inline' https://cdn.materialdesignicons.com " +
			"https://cloud.typography.com https://fast.fonts.net " +
			"https://fonts.googleapis.com https://maxcdn.bootstrapcdn.com " +
			"https://p.typekit.net https://pro.fontawesome.com " +
			"https://use.fontawesome.com https://use.typekit.net;" +
			"object-src 'none'")
	return newCsp.String()
}

func (this *Signer) genCertURL(cert *x509.Certificate, signURL *url.URL) (*url.URL, error) {
	var baseURL *url.URL
	if this.overrideBaseURL != nil {
		baseURL = this.overrideBaseURL
	} else {
		baseURL = signURL
	}
	urlPath := path.Join(util.CertURLPrefix, url.PathEscape(util.CertName(cert)))
	certHRef, err := url.Parse(urlPath)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing cert URL %q", urlPath)
	}
	ret := baseURL.ResolveReference(certHRef)
	return ret, nil
}

// promGatewayRequestsTotal is a Prometheus counter that observes total gateway requests count.
var promGatewayRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "total_gateway_requests_by_code",
		Help: "Total number of underlying requests to AMP document server - by HTTP response status code.",
	},
	[]string{"code"},
)

// promGatewayRequestsLatency is a Prometheus summary that observes requests latencies.
// Objectives key value pairs set target quantiles and respective allowed rank variance.
// Upon query, for each Objective quantile (0.5, 0.9, 0.99) the summary returns
// an actual observed latency value that is ranked close to the Objective value.
// For more intuition on the Objectives see http://alexandrutopliceanu.ro/post/targeted-quantiles/.
var promGatewayRequestsLatency = promauto.NewSummaryVec(
	prometheus.SummaryOpts{
		Name:       "gateway_request_latencies_in_seconds",
		Help:       "Latencies (in seconds) of gateway requests to AMP document server - by HTTP response status code.",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	},
	[]string{"code"},
)

func (this *Signer) fetchURLAndMeasure(fetch *url.URL, serveHTTPReq *http.Request) (*http.Request, *http.Response, *util.HTTPError) {
	startTime := this.timeNow()

	fetchReq, fetchResp, httpErr := this.fetchURL(fetch, serveHTTPReq)
	if httpErr == nil {
		// httpErr is nil, i.e. the gateway request did succeed. Let Prometheus
		// observe the gateway request and its latency - along with the response code.
		label := prometheus.Labels{"code": strconv.Itoa(fetchResp.StatusCode)}
		promGatewayRequestsTotal.With(label).Inc()

		latency := this.timeNow().Sub(startTime)
		promGatewayRequestsLatency.With(label).Observe(latency.Seconds())
	} else {
		// httpErr can have a non-nil value. E.g. http.StatusBadGateway (502)
		// is the most probable error fetchURL returns if failed. In case of
		// non-nil httpErr don't observe the request. Instead do nothing and let
		// mux's promRequestsTotal observe the top level non-gateway request (along
		// with the response code e.g. 502) once signer has completed handling it.
	}

	return fetchReq, fetchResp, httpErr
}

func (this *Signer) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Add("Vary", "Accept, AMP-Cache-Transform")

	if err := req.ParseForm(); err != nil {
		util.NewHTTPError(http.StatusBadRequest, "Form input parsing failed: ", err).LogAndRespond(resp)
		return
	}
	var fetch, sign string
	params := mux.Params(req)
	if inPathSignURL := params["signURL"]; inPathSignURL != "" {
		sign = inPathSignURL
	} else {
		if len(req.Form["fetch"]) > 1 {
			util.NewHTTPError(http.StatusBadRequest, "More than 1 fetch param").LogAndRespond(resp)
			return
		}
		if len(req.Form["sign"]) != 1 {
			util.NewHTTPError(http.StatusBadRequest, "Not exactly 1 sign param").LogAndRespond(resp)
			return
		}
		fetch = req.FormValue("fetch")
		sign = req.FormValue("sign")
	}
	fetchURL, signURL, errorOnStatefulHeaders, httpErr := parseURLs(fetch, sign, this.urlSets)
	if httpErr != nil {
		httpErr.LogAndRespond(resp)
		return
	}

	fetchReq, fetchResp, httpErr := this.fetchURLAndMeasure(fetchURL, req)
	if httpErr != nil {
		httpErr.LogAndRespond(resp)
		return
	}

	defer func() {
		if err := fetchResp.Body.Close(); err != nil {
			log.Println("Error closing fetchResp body:", err)
		}
	}()

	if err := this.shouldPackage(); err != nil {
		log.Println("Not packaging because server is unhealthy; see above log statements.", err)
		proxyUnconsumed(resp, fetchResp)
		return
	}
	var act string
	var transformVersion int64
	if this.requireHeaders {
		header_value := GetJoined(req.Header, "AMP-Cache-Transform")
		act, transformVersion = amp_cache_transform.ShouldSendSXG(header_value)
		if act == "" {
			log.Println("Not packaging because AMP-Cache-Transform request header is invalid:", header_value)
			proxyUnconsumed(resp, fetchResp)
			return
		}
	} else {
		var err error
		transformVersion, err = transformer.SelectVersion(nil)
		if err != nil {
			log.Println("Not packaging because of internal SelectVersion error:", err)
			proxyUnconsumed(resp, fetchResp)
			return
		}
	}
	if this.requireHeaders && !accept.CanSatisfy(GetJoined(req.Header, "Accept")) {
		log.Printf("Not packaging because Accept request header lacks application/signed-exchange;v=%s.\n", accept.AcceptedSxgVersion)
		proxyUnconsumed(resp, fetchResp)
		return
	}

	switch fetchResp.StatusCode {
	case 200:
		// If fetchURL returns an OK status, then validate, munge, and package.
		if err := validateFetch(fetchReq, fetchResp); err != nil {
			log.Println("Not packaging because of invalid fetch: ", err)
			proxyUnconsumed(resp, fetchResp)
			return
		}
		for header := range statefulResponseHeaders {
			if errorOnStatefulHeaders && GetJoined(fetchResp.Header, header) != "" {
				log.Println("Not packaging because ErrorOnStatefulHeaders = True and fetch response contains stateful header: ", header)
				proxyUnconsumed(resp, fetchResp)
				return
			}
		}

		if fetchResp.Header.Get("Variants") != "" || fetchResp.Header.Get("Variant-Key") != "" ||
			// Include versioned headers per https://github.com/WICG/webpackage/pull/406.
			fetchResp.Header.Get("Variants-04") != "" || fetchResp.Header.Get("Variant-Key-04") != "" {
			// Variants headers (https://tools.ietf.org/html/draft-ietf-httpbis-variants-04) are disallowed by AMP Cache.
			// We could delete the headers, but it's safest to assume they reflect the downstream server's intent.
			log.Println("Not packaging because response contains a Variants header.")
			proxyUnconsumed(resp, fetchResp)
			return
		}

		this.consumeAndSign(resp, fetchResp, &SXGParams{signURL, act, transformVersion})

	case 304:
		// If fetchURL returns a 304, then also return a 304 with appropriate headers.
		for header := range statusNotModifiedHeaders {
			if value := GetJoined(fetchResp.Header, header); value != "" {
				resp.Header().Set(header, value)
			}
		}
		resp.WriteHeader(http.StatusNotModified)

	default:
		log.Printf("Not packaging because status code %d is unrecognized.\n", fetchResp.StatusCode)
		proxyUnconsumed(resp, fetchResp)
	}
}

func formatLinkHeader(preloads []*rpb.Metadata_Preload) (string, error) {
	var values []string
	for _, preload := range preloads {
		u, err := url.Parse(preload.Url)
		if err != nil {
			return "", errors.Wrapf(err, "Invalid preload URL: %q\n", preload.Url)
		}
		// Percent-escape any characters in the query that aren't valid
		// URL characters (but don't escape '=' or '&').
		u.RawQuery = url.PathEscape(u.RawQuery)

		if preload.As == "" {
			return "", errors.Errorf("Missing `as` attribute for preload URL: %q\n", preload.Url)
		}

		valid := true
		var value strings.Builder
		value.WriteByte('<')
		value.WriteString(u.String())
		value.WriteString(">;rel=preload;as=")
		value.WriteString(preload.As)
		for _, attr := range preload.GetAttributes() {
			value.WriteByte(';')
			value.WriteString(attr.Key)
			value.WriteByte('=')
			quotedVal, err := util.QuotedString(attr.Val)
			if err != nil {
				valid = false
				break
			}
			value.WriteString(quotedVal)
		}
		if valid {
			values = append(values, value.String())
		}
	}
	return strings.Join(values, ","), nil
}

type SXGParams struct {
	signURL                 *url.URL
	ampCacheTransformHeader string
	transformVersion        int64
}

// consumedFetchResp stores the fetch response in memory - including the
// consumed body, not a stream reader. Signer loads the whole payload in memory
// in order to be able to sign it, because it's required by signer's
// serveSignedExchange method, specifically by its underlying calls to
// transformer.Process and to signedexchange.NewExchange. The former performs
// AMP HTML transforms, which depend on a non-streaming HTML parser. The latter
// signs it, which requires the whole payload in memory, because MICE requires
// the sender to process its payload in reverse order
// (https://tools.ietf.org/html/draft-thomson-http-mice-03#section-2.1). In an
// HTTP reverse proxy, this could be done using range requests, but would be
// inefficient.
type consumedFetchResp struct {
	body       []byte
	StatusCode int
	Header     http.Header
}

// maxSignableBodyLength is the signable payload length limit. If not hit, the
// signer will load the payload into consumedFetchResp and sign it. If hit, the
// signer won't sign the payload, but will proxy it in full by streaming it.
// This way the signer limits per-request memory usage, making amppackager more
// predictable provisioning-wise. The limit is mostly arbitrary, though there's
// no benefit to having a limit greater than that of AMP Caches.
const maxSignableBodyLength = 4 * 1 << 20

func (this *Signer) consumeAndSign(resp http.ResponseWriter, fetchResp *http.Response, params *SXGParams) {
	// Cap in order to limit per-request memory usage.
	fetchBodyMaybeCapped, err := ioutil.ReadAll(io.LimitReader(fetchResp.Body, maxSignableBodyLength))
	if err != nil {
		util.NewHTTPError(http.StatusBadGateway, "Error reading body: ", err).LogAndRespond(resp)
		return
	}

	if len(fetchBodyMaybeCapped) == maxSignableBodyLength {
		// Body was too long and has been capped. Fallback to proxying.
		log.Println("Not packaging because the document size hit the limit of ", strconv.Itoa(maxSignableBodyLength), " bytes.")
		proxyPartiallyConsumed(resp, fetchResp, fetchBodyMaybeCapped)
	} else {
		// Body has been consumed fully. OK to proceed.
		this.serveSignedExchange(resp, consumedFetchResp{fetchBodyMaybeCapped, fetchResp.StatusCode, fetchResp.Header}, params)
	}

}

var promSignedAmpDocumentsSize = promauto.NewSummaryVec(
	prometheus.SummaryOpts{
		Name:       "signed_amp_documents_size_in_bytes",
		Help:       "Actual size (in bytes) of gateway response body from AMP document server. Reported only if signer decided to sign, not return an error or proxy unsigned.",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	},
	[]string{},
)

var promDocumentsSignedVsUnsigned = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "documents_signed_vs_unsigned",
		Help: "Total number of successful underlying requests to AMP document server, broken down by status based on the action signer has taken: sign or proxy unsigned.",
	},
	[]string{"status"},
)

// serveSignedExchange does the actual work of transforming, packaging, signing and writing to the response.
func (this *Signer) serveSignedExchange(resp http.ResponseWriter, fetchResp consumedFetchResp, params *SXGParams) {
	// Perform local transformations, as required by AMP SXG caches, per
	// docs/cache_requirements.md.
	r := getTransformerRequest(this.rtvCache, string(fetchResp.body), params.signURL.String())
	r.Version = params.transformVersion
	transformed, metadata, err := transformer.Process(r)
	if err != nil {
		log.Println("Not packaging due to transformer error:", err)
		proxyConsumed(resp, fetchResp)
		return
	}

	// Validate and format Link header.
	linkHeader, err := formatLinkHeader(metadata.Preloads)
	if err != nil {
		log.Println("Not packaging due to Link header error:", err)
		proxyConsumed(resp, fetchResp)
		return
	}

	// Begin mutations on original fetch response. From this point forward, do
	// not fall-back to proxy().

	// Remove stateful headers.
	for header := range statefulResponseHeaders {
		fetchResp.Header.Del(header)
	}

	// Set Link header if formatting returned a valid value, otherwise, delete
	// it to ensure there are no privacy-violating Link:rel=preload headers.
	if linkHeader != "" {
		fetchResp.Header.Set("Link", linkHeader)
	} else {
		fetchResp.Header.Del("Link")
	}

	// Set content length.
	fetchResp.Header.Set("Content-Length", strconv.Itoa(len(transformed)))

	// Set general security headers.
	fetchResp.Header.Set("X-Content-Type-Options", "nosniff")

	// Mutate the fetched CSP to make sure it cannot break AMP pages.
	fetchResp.Header.Set(
		"Content-Security-Policy",
		MutateFetchedContentSecurityPolicy(
			fetchResp.Header.Get("Content-Security-Policy")))

	exchange := signedexchange.NewExchange(
		accept.SxgVersion,
		/*uri=*/ params.signURL.String(),
		/*method=*/ "GET",
		http.Header{}, fetchResp.StatusCode, fetchResp.Header, []byte(transformed))
	if err := exchange.MiEncodePayload(miRecordSize); err != nil {
		log.Printf("Error MI-encoding: %s\n", err)
		proxyConsumed(resp, fetchResp)
		return
	}
	cert := this.certHandler.GetLatestCert()
	certURL, err := this.genCertURL(cert, params.signURL)
	if err != nil {
		log.Printf("Error building cert URL: %s\n", err)
		proxyConsumed(resp, fetchResp)
		return
	}
	now := time.Now()
	validityHRef, err := url.Parse(util.ValidityMapPath)
	if err != nil {
		// Won't ever happen because util.ValidityMapPath is a constant.
		log.Printf("Error building validity href: %s\n", err)
		proxyConsumed(resp, fetchResp)
		return
	}
	// Expires - Date must be <= 604800 seconds, per
	// https://tools.ietf.org/html/draft-yasskin-httpbis-origin-signed-exchanges-impl-00#section-3.5.
	duration := 7 * 24 * time.Hour
	if maxAge := time.Duration(metadata.MaxAgeSecs) * time.Second; maxAge < duration {
		duration = maxAge
	}
	date := now.Add(-24 * time.Hour)
	expires := date.Add(duration)
	if !expires.After(now) {
		log.Printf("Not packaging because computed max-age %d places expiry in the past\n", metadata.MaxAgeSecs)
		proxyConsumed(resp, fetchResp)
		return
	}
	signer := signedexchange.Signer{
		Date:        date,
		Expires:     expires,
		Certs:       []*x509.Certificate{cert},
		CertUrl:     certURL,
		ValidityUrl: params.signURL.ResolveReference(validityHRef),
		PrivKey:     this.key,
		// TODO(twifkak): Should we make Rand user-configurable? The
		// default is to use getrandom(2) if available, else
		// /dev/urandom.
	}
	if err := exchange.AddSignatureHeader(&signer); err != nil {
		log.Printf("Error signing exchange: %s\n", err)
		proxyConsumed(resp, fetchResp)
		return
	}
	var body bytes.Buffer
	if err := exchange.Write(&body); err != nil {
		log.Printf("Error serializing exchange: %s\n", err)
		proxyConsumed(resp, fetchResp)
		return
	}

	// If requireHeaders was true when constructing signer, the
	// AMP-Cache-Transform outer response header is required (and has already
	// been validated)
	if params.ampCacheTransformHeader != "" {
		resp.Header().Set("AMP-Cache-Transform", params.ampCacheTransformHeader)
	}

	resp.Header().Set("Content-Type", accept.SxgContentType)
	// We set a zero freshness lifetime on the SXG, so that naive caching
	// intermediaries won't inhibit the update of this resource on AMP
	// caches. AMP caches are recommended to base their update strategies
	// on a combination of inner and outer resource lifetime.
	//
	// If you change this code to set a Cache-Control based on the inner
	// resource, you need to ensure that its max-age is no longer than the
	// lifetime of the signature (6 days, per above). Maybe an even tighter
	// bound than that, based on data about client clock skew.
	resp.Header().Set("Cache-Control", "no-transform, max-age=0")
	resp.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := resp.Write(body.Bytes()); err != nil {
		log.Println("Error writing response:", err)
		return
	}

	promSignedAmpDocumentsSize.WithLabelValues().Observe(float64(len(fetchResp.body)))
	promDocumentsSignedVsUnsigned.WithLabelValues("signed").Inc()
}

func proxyUnconsumed(resp http.ResponseWriter, fetchResp *http.Response) {
	proxyImpl(resp, fetchResp.Header, fetchResp.StatusCode,
		/* consumedPrefix= */ nil,
		/* unconsumedSuffix = */ fetchResp.Body)
}

func proxyPartiallyConsumed(resp http.ResponseWriter, fetchResp *http.Response, consumedBodyPrefix []byte) {
	proxyImpl(resp, fetchResp.Header, fetchResp.StatusCode,
		/* consumedPrefix= */ consumedBodyPrefix,
		/* unconsumedSuffix = */ fetchResp.Body)
}

func proxyConsumed(resp http.ResponseWriter, consumedFetchResp consumedFetchResp) {
	proxyImpl(resp, consumedFetchResp.Header, consumedFetchResp.StatusCode,
		/* consumedPrefix= */ consumedFetchResp.body,
		/* unconsumedSuffix = */ nil)
}

// Proxy the content unsigned. The body may be already partially or fully
// consumed. TODO(twifkak): Take a look at the source code to
// httputil.ReverseProxy and see what else needs to be implemented.
func proxyImpl(resp http.ResponseWriter, header http.Header, statusCode int, consumedPrefix []byte, unconsumedSuffix io.ReadCloser) {
	for k, v := range header {
		resp.Header()[k] = v
	}
	resp.WriteHeader(statusCode)

	if consumedPrefix != nil {
		resp.Write(consumedPrefix)
	}

	if unconsumedSuffix != nil {
		bytesCopied, err := io.Copy(resp, unconsumedSuffix)
		if err != nil {
			if bytesCopied == 0 {
				util.NewHTTPError(http.StatusInternalServerError, "Error copying response body").LogAndRespond(resp)
			} else {
				log.Printf("Error copying response body, %d bytes into stream\n", bytesCopied)
			}
		}
	}

	promDocumentsSignedVsUnsigned.WithLabelValues("proxied unsigned").Inc()
}
