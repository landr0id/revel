package revel

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/robfig/pathtree"
	"gopkg.in/v1/yaml"
)

type Route struct {
	Method         string   // e.g. GET
	Path           string   // e.g. /app/:id
	Action         string   // e.g. "Application.ShowApp", "404"
	ControllerName string   // e.g. "Application", ""
	MethodName     string   // e.g. "ShowApp", ""
	FixedParams    []string // e.g. "arg1","arg2","arg3" (CSV formatting)
	TreePath       string   // e.g. "/GET/app/:id"

	routesPath string // e.g. /Users/robfig/gocode/src/myapp/conf/routes
	line       int    // e.g. 3
}

type RouteMatch struct {
	Action         string // e.g. 404
	ControllerName string // e.g. Application
	MethodName     string // e.g. ShowApp
	FixedParams    []string
	Params         map[string][]string // e.g. {id: 123}
}

type arg struct {
	name       string
	index      int
	constraint *regexp.Regexp
}

// Prepares the route to be used in matching.
func NewRoute(method, path, action, routesPath string, fixedArgs []string) (r *Route) {
	r = &Route{
		Method:      strings.ToUpper(method),
		Path:        path,
		Action:      action,
		FixedParams: fixedArgs,
		TreePath:    treePath(strings.ToUpper(method), path),
		routesPath:  routesPath,
		line:        0,
	}

	// URL pattern
	if !strings.HasPrefix(r.Path, "/") {
		ERROR.Print("Absolute URL required.")
		return
	}

	actionSplit := strings.Split(action, ".")
	if len(actionSplit) == 2 {
		r.ControllerName = actionSplit[0]
		r.MethodName = actionSplit[1]
	}

	return
}

func treePath(method, path string) string {
	if method == "*" {
		method = ":METHOD"
	}
	return "/" + method + path
}

type Router struct {
	Routes []*Route
	Tree   *pathtree.Node
	path   string // path to the routes file
}

var notFound = &RouteMatch{Action: "404"}

func (router *Router) Route(req *http.Request) *RouteMatch {
	leaf, expansions := router.Tree.Find(treePath(req.Method, req.URL.Path))
	if leaf == nil {
		return nil
	}
	route := leaf.Value.(*Route)

	// Create a map of the route parameters.
	var params url.Values
	if len(expansions) > 0 {
		params = make(url.Values)
		for i, v := range expansions {
			params[leaf.Wildcards[i]] = []string{v}
		}
	}

	// Special handling for explicit 404's.
	if route.Action == "404" {
		return notFound
	}

	// If the action is variablized, replace into it with the captured args.
	controllerName, methodName := route.ControllerName, route.MethodName
	if controllerName[0] == ':' {
		controllerName = params[controllerName[1:]][0]
	}
	if methodName[0] == ':' {
		methodName = params[methodName[1:]][0]
	}

	return &RouteMatch{
		ControllerName: controllerName,
		MethodName:     methodName,
		Params:         params,
		FixedParams:    route.FixedParams,
	}
}

// Refresh re-reads the routes file and re-calculates the routing table.
// Returns an error if a specified action could not be found.
func (router *Router) Refresh() (err *Error) {
	router.Routes, err = parseRoutesFile(router.path, "", true)
	if err != nil {
		return
	}
	err = router.updateTree()
	return
}

func (router *Router) updateTree() *Error {
	router.Tree = pathtree.New()
	for _, route := range router.Routes {
		err := router.Tree.Add(route.TreePath, route)

		// Allow GETs to respond to HEAD requests.
		if err == nil && route.Method == "GET" {
			err = router.Tree.Add(treePath("HEAD", route.Path), route)
		}

		// Error adding a route to the pathtree.
		if err != nil {
			return routeError(err, route.routesPath, "", 0)
		}
	}
	return nil
}

// parseRoutesFile reads the given routes file and returns the contained routes.
func parseRoutesFile(routesPath, joinedPath string, validate bool) ([]*Route, *Error) {
	contentBytes, err := ioutil.ReadFile(routesPath)
	if err != nil {
		return nil, &Error{
			Title:       "Failed to load routes file",
			Description: err.Error(),
		}
	}
	return parseRoutes(routesPath, joinedPath, string(contentBytes), validate)
}

var (
	routeMethodPattern   *regexp.Regexp = regexp.MustCompile("(?i)^(GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD|WS|\\*)$")
	requiredRouteOptions []string       = []string{"method", "path", "action"}
)

func lineFromYamlError(err error) int {
	if strings.Contains(err.Error(), "YAML") {
		strconv.Atoi(err.Error()[17:])
	}
	return 0
}

// parseRoutes reads the content of a routes file into the routing table.
// joinedPath is the recursively passed in prefix for routes.
func parseRoutes(routesPath, joinedPath, content string, validate bool) ([]*Route, *Error) {
	var (
		routes     []*Route
		parsedYaml []map[string]interface{}
	)

	if err := yaml.Unmarshal([]byte(content), &parsedYaml); err != nil {
		return nil, routeError(err, routesPath, content, lineFromYamlError(err))
	}

	for _, route := range parsedYaml {
		if route == nil {
			continue
		}

		// If this is a module import...
		if moduleName, ok := route["import"].(string); ok {
			var prefix string
			if prefix, ok = route["prefix"].(string); !ok {
				prefix = ""
			}

			// this will avoid accidental double forward slashes in a route.
			// this also avoids pathtree freaking out and causing a runtime panic
			// because of the double slashes
			if strings.HasSuffix(joinedPath, "/") && strings.HasPrefix(prefix, "/") {
				joinedPath = joinedPath[0 : len(joinedPath)-1]
			}
			modulePrefix := strings.Join([]string{joinedPath, prefix}, "")

			moduleRoutes, err := getModuleRoutes(moduleName, modulePrefix, validate)
			if err != nil {
				return nil, routeError(err, routesPath, content, lineFromYamlError(err))
			}

			routes = append(routes, moduleRoutes...)
		} else {
			// This should be a valid route of format:
			//	- method: (GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD|WS|\\*)
			//	- path: /example/path
			//	- action: <Controller>.<Action>
			//	- params: ["first", "second"]

			// Verify all of the required keys are present. "params" is optional
			for _, key := range requiredRouteOptions {
				if _, ok := route[key]; !ok {
					return nil, routeError(errors.New(fmt.Sprintf("Missing required route option \"%s\"", key)), routesPath, content, 0)
				}
			}

			// this will be nil if there are no params, but this needs to be a string slice
			var params []string
			_, ok := route["params"]
			if ok {
				for _, val := range route["params"].([]interface{}) {
					params = append(params, val.(string))
				}
			}

			method := route["method"].(string)
			if !routeMethodPattern.MatchString(method) {
				return nil, routeError(errors.New(fmt.Sprintf("Unknown route method \"%s\"", method)), routesPath, content, 0)
			}
			path := route["path"].(string)
			action := route["action"].(string)

			route := NewRoute(method, path, action, routesPath, params)
			routes = append(routes, route)

			if validate {
				if err := validateRoute(route); err != nil {
					return nil, routeError(err, routesPath, content, 0)
				}
			}
		}
	}

	return routes, nil
}

// validateRoute checks that every specified action exists.
func validateRoute(route *Route) error {
	// Skip 404s
	if route.Action == "404" {
		return nil
	}

	// We should be able to load the action.
	parts := strings.Split(route.Action, ".")
	if len(parts) != 2 {
		return fmt.Errorf("Expected two parts (Controller.Action), but got %d: %s",
			len(parts), route.Action)
	}

	// Skip variable routes.
	if parts[0][0] == ':' || parts[1][0] == ':' {
		return nil
	}

	var c Controller
	if err := c.SetAction(parts[0], parts[1]); err != nil {
		return err
	}

	return nil
}

// routeError adds context to a simple error message.
func routeError(err error, routesPath, content string, line int) *Error {
	if revelError, ok := err.(*Error); ok {
		return revelError
	}
	// Load the route file content if necessary
	if content == "" {
		contentBytes, err := ioutil.ReadFile(routesPath)
		if err != nil {
			ERROR.Printf("Failed to read route file %s: %s\n", routesPath, err)
		} else {
			content = string(contentBytes)
		}
	}
	return &Error{
		Title:       "Route validation error",
		Description: err.Error(),
		Path:        routesPath,
		Line:        line,
		SourceLines: strings.Split(content, "\n"),
	}
}

// getModuleRoutes loads the routes file for the given module and returns the
// list of routes.
func getModuleRoutes(moduleName, joinedPath string, validate bool) ([]*Route, *Error) {
	// Look up the module.  It may be not found due to the common case of e.g. the
	// testrunner module being active only in dev mode.
	module, found := ModuleByName(moduleName)
	if !found {
		INFO.Println("Skipping routes for inactive module", moduleName)
		return nil, nil
	}
	return parseRoutesFile(path.Join(module.Path, "conf", "routes.yml"), joinedPath, validate)
}

func NewRouter(routesPath string) *Router {
	return &Router{
		Tree: pathtree.New(),
		path: routesPath,
	}
}

type ActionDefinition struct {
	Host, Method, Url, Action string
	Star                      bool
	Args                      map[string]string
}

func (a *ActionDefinition) String() string {
	return a.Url
}

func (router *Router) Reverse(action string, argValues map[string]string) *ActionDefinition {
	actionSplit := strings.Split(action, ".")
	if len(actionSplit) != 2 {
		ERROR.Print("revel/router: reverse router got invalid action ", action)
		return nil
	}
	controllerName, methodName := actionSplit[0], actionSplit[1]

	for _, route := range router.Routes {
		// Skip routes without either a ControllerName or MethodName
		if route.ControllerName == "" || route.MethodName == "" {
			continue
		}

		// Check that the action matches or is a wildcard.
		controllerWildcard := route.ControllerName[0] == ':'
		methodWildcard := route.MethodName[0] == ':'
		if (!controllerWildcard && route.ControllerName != controllerName) ||
			(!methodWildcard && route.MethodName != methodName) {
			continue
		}
		if controllerWildcard {
			argValues[route.ControllerName[1:]] = controllerName
		}
		if methodWildcard {
			argValues[route.MethodName[1:]] = methodName
		}

		// Build up the URL.
		var (
			queryValues  = make(url.Values)
			pathElements = strings.Split(route.Path, "/")
		)
		for i, el := range pathElements {
			if el == "" || el[0] != ':' {
				continue
			}

			val, ok := argValues[el[1:]]
			if !ok {
				val = "<nil>"
				ERROR.Print("revel/router: reverse route missing route arg ", el[1:])
			}
			pathElements[i] = val
			delete(argValues, el[1:])
			continue
		}

		// Add any args that were not inserted into the path into the query string.
		for k, v := range argValues {
			queryValues.Set(k, v)
		}

		// Calculate the final URL and Method
		url := strings.Join(pathElements, "/")
		if len(queryValues) > 0 {
			url += "?" + queryValues.Encode()
		}

		method := route.Method
		star := false
		if route.Method == "*" {
			method = "GET"
			star = true
		}

		return &ActionDefinition{
			Url:    url,
			Method: method,
			Star:   star,
			Action: action,
			Args:   argValues,
			Host:   "TODO",
		}
	}
	ERROR.Println("Failed to find reverse route:", action, argValues)
	return nil
}

func init() {
	OnAppStart(func() {
		MainRouter = NewRouter(path.Join(BasePath, "conf", "routes.yml"))
		if MainWatcher != nil && Config.BoolDefault("watch.routes", true) {
			MainWatcher.Listen(MainRouter, MainRouter.path)
		} else {
			MainRouter.Refresh()
		}
	})
}

func RouterFilter(c *Controller, fc []Filter) {
	// Figure out the Controller/Action
	var route *RouteMatch = MainRouter.Route(c.Request.Request)
	if route == nil {
		c.Result = c.NotFound("No matching route found: " + c.Request.RequestURI)
		return
	}

	// The route may want to explicitly return a 404.
	if route.Action == "404" {
		c.Result = c.NotFound("(intentionally)")
		return
	}

	// Set the action.
	if err := c.SetAction(route.ControllerName, route.MethodName); err != nil {
		c.Result = c.NotFound(err.Error())
		return
	}

	// Add the route and fixed params to the Request Params.
	c.Params.Route = route.Params

	// Add the fixed parameters mapped by name.
	// TODO: Pre-calculate this mapping.
	for i, value := range route.FixedParams {
		if c.Params.Fixed == nil {
			c.Params.Fixed = make(url.Values)
		}
		if i < len(c.MethodType.Args) {
			arg := c.MethodType.Args[i]
			c.Params.Fixed.Set(arg.Name, value)
		} else {
			WARN.Println("Too many parameters to", route.Action, "trying to add", value)
			break
		}
	}

	fc[0](c, fc[1:])
}
