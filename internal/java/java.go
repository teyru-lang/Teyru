// Package java prints a Teyru program as the Java source it stands for.
//
// It is the other half of the JDK differential: the compiler's Java semantics
// are checked against javac's answers by compiling one spelling of a program
// with each, so something has to turn the Teyru spelling into Java's. That is
// this package, and what it is worth is exactly what it can translate: a
// Teyru-only feature -- a native property, the Lombok or Spring or Gson shaped
// layers, the parts of the standard library Java has no class of its own for --
// is refused by name, with the reason, rather than translated into something
// that only looks like the same program.
//
// The printer works on the syntax tree the parser produces, before the checker
// has run: what it prints is what the source says, which is what makes the
// comparison with javac a comparison of two implementations of one program
// instead of a comparison of two lowerings of it.
package java

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
)

// DiagCode is the diagnostic every refusal is reported with.
const DiagCode = "TY-INT-0102"

// A Refusal is one Teyru feature the printer cannot express as Java, and why.
// The pair is the point: "cannot translate" without a reason is not a report,
// it is a shrug.
type Refusal struct {
	Pos     source.Pos
	Feature string
	Reason  string
}

// Message is the one-line diagnostic text.
func (r Refusal) Message() string {
	return fmt.Sprintf("cannot emit Java for %s: %s", r.Feature, r.Reason)
}

func (r Refusal) String() string { return r.Pos.String() + ": " + r.Message() }

// A Result is the Java text of a program, together with the names the JDK needs
// to run it and everything that stood in the way.
type Result struct {
	// Text is the Java source. It is only meaningful when Refusals is empty:
	// a program with a refused construct has no Java spelling.
	Text string
	// Entry is the class that holds `main`, which is the class to run.
	Entry string
	// File is the name the source file must have. javac insists a public type
	// lives in a file named after it, so this is the public type's name when
	// there is exactly one.
	File string
	// Refusals lists what could not be translated, in source order.
	Refusals []Refusal
}

// OK reports whether the whole program could be translated.
func (r *Result) OK() bool { return len(r.Refusals) == 0 }

// Translate renders the program's files as one Java compilation unit.
//
// The files are the program's own; the standard library is not among them (it
// is translated by name, through the table below), so the caller passes exactly
// what the program consists of.
func Translate(files []*ast.File) *Result {
	p := &printer{
		out:     &strings.Builder{},
		imports: map[string]bool{},
		decl:    map[string]bool{},
	}
	p.entry, p.fileName = entryOf(files)
	for _, f := range files {
		for _, d := range f.Types {
			p.decl[d.Name] = true
		}
	}
	body := p.capture(func() {
		for _, f := range files {
			p.file(f)
			p.types(f)
		}
	})
	return &Result{
		Text:     strings.TrimRight(p.finish(body), "\n") + "\n",
		Entry:    p.entry,
		File:     p.fileName,
		Refusals: p.refusals,
	}
}

// ---------------------------------------------------------------- the table
//
// javaOf maps a standard-library class name to the java.* class of the same
// name, which is the one whose API the Teyru class is a copy of. Everything
// listed here is translated; everything the standard library has that Java
// does not is in refuseStdlib below and is refused by name.
var javaOf = map[string]string{}

// refuseStdlib maps a standard-library class name with no java.* equivalent to
// the reason, which is also the classification the report groups by.
var refuseStdlib = map[string]string{}

func init() {
	// java.lang: no import is needed for any of these, and the name is the
	// same on both sides, so translating one is a no-op that only says the
	// name is known.
	for _, n := range []string{
		"Object", "String", "StringBuilder", "StringBuffer", "Math", "System",
		"Throwable", "Exception", "RuntimeException", "Error",
		"NullPointerException", "IndexOutOfBoundsException",
		"ArrayIndexOutOfBoundsException", "StringIndexOutOfBoundsException",
		"ArithmeticException", "ClassCastException", "IllegalArgumentException",
		"NumberFormatException", "IllegalStateException", "IllegalMonitorStateException",
		"NegativeArraySizeException", "AssertionError",
		"ArrayStoreException", "UnsupportedOperationException", "StackOverflowError",
		"OutOfMemoryError", "Class", "Number", "Integer", "Long", "Short", "Byte",
		"Character", "Double", "Float", "Boolean", "Void", "Cloneable",
		"Comparable", "AutoCloseable", "Iterable", "Runnable", "Thread", "Enum",
		"Record", "CharSequence", "ReflectiveOperationException",
		"ClassNotFoundException", "IllegalAccessException", "InstantiationException",
		"InvocationTargetException", "NoSuchMethodException", "NoSuchFieldException",
		"InterruptedException", "CloneNotSupportedException", "Deprecated",
		"Override", "SuppressWarnings", "SafeVarargs", "FunctionalInterface",
	} {
		javaOf[n] = "java.lang." + n
	}
	// java.util and the rest: these are the ones the printer has to import.
	for _, n := range []string{
		"List", "ArrayList", "LinkedList", "Map", "HashMap", "LinkedHashMap",
		"TreeMap", "Set", "HashSet", "LinkedHashSet", "TreeSet", "Collection",
		"Iterator", "ListIterator", "SortedMap", "NavigableMap", "SortedSet",
		"NavigableSet", "Queue", "Deque", "ArrayDeque", "Arrays", "Collections",
		"Objects", "Optional", "OptionalInt", "OptionalLong", "OptionalDouble",
		"UUID", "Random", "StringTokenizer", "BitSet", "Base64", "HexFormat",
		"Comparator", "Spliterator", "StringJoiner", "Enumeration",
		"NoSuchElementException", "InputMismatchException",
		"Scanner", "Date", "Calendar", "TimeZone",
	} {
		javaOf[n] = "java.util." + n
	}
	for _, n := range []string{
		"NumberFormat", "DecimalFormat", "DateFormat", "SimpleDateFormat",
		"MessageFormat", "DateFormatSymbols", "DecimalFormatSymbols",
		"ParsePosition", "ChoiceFormat", "Format", "ParseException",
	} {
		javaOf[n] = "java.text." + n
	}
	javaOf["Spliterators"] = "java.util.Spliterators"
	javaOf["Random"] = "java.util.Random"
	for _, n := range []string{
		"Stream", "IntStream", "LongStream", "DoubleStream", "Collector", "Collectors",
	} {
		javaOf[n] = "java.util.stream." + n
	}
	javaOf["StreamSupport"] = "java.util.stream.StreamSupport"
	for _, n := range []string{
		"Function", "BiFunction", "Consumer", "Supplier", "Predicate",
		"UnaryOperator", "BinaryOperator", "BiConsumer", "BiPredicate",
	} {
		javaOf[n] = "java.util.function." + n
	}
	for _, n := range []string{
		"IntFunction", "IntConsumer", "IntPredicate", "IntSupplier",
		"IntUnaryOperator", "IntBinaryOperator",
		"LongFunction", "LongConsumer", "LongPredicate", "LongSupplier",
		"LongUnaryOperator", "LongBinaryOperator",
		"DoubleFunction", "DoubleConsumer", "DoublePredicate", "DoubleSupplier",
		"DoubleUnaryOperator", "DoubleBinaryOperator",
		"ToIntFunction", "ToLongFunction", "ToDoubleFunction",
		"ObjIntConsumer", "ObjLongConsumer", "ObjDoubleConsumer",
	} {
		javaOf[n] = "java.util.function." + n
	}
	for _, n := range []string{"Pattern", "Matcher", "MatchResult", "PatternSyntaxException"} {
		javaOf[n] = "java.util.regex." + n
	}
	for _, n := range []string{"BigInteger", "BigDecimal", "MathContext", "RoundingMode"} {
		javaOf[n] = "java.math." + n
	}
	for _, n := range []string{
		"LocalDate", "LocalTime", "LocalDateTime", "Instant", "Duration", "Period",
		"ZoneId", "ZoneOffset", "ZonedDateTime", "DayOfWeek", "Month",
		"DateTimeException", "UnsupportedTemporalTypeException", "Clock",
		"MonthDay", "YearMonth", "Year", "OffsetDateTime", "OffsetTime",
	} {
		javaOf[n] = "java.time." + n
	}
	for _, n := range []string{"DateTimeFormatter", "DateTimeParseException", "FormatStyle", "ResolverStyle"} {
		javaOf[n] = "java.time.format." + n
	}
	for _, n := range []string{"ZoneOffsetTransition", "ZoneRules", "ZoneRulesException"} {
		javaOf[n] = "java.time.zone." + n
	}
	for _, n := range []string{
		"IOException", "EOFException", "UncheckedIOException", "PrintStream",
		"PrintWriter", "InputStream", "OutputStream", "Reader", "Writer",
		"BufferedReader", "BufferedWriter", "BufferedInputStream",
		"BufferedOutputStream", "DataInputStream", "DataOutputStream",
		"ByteArrayInputStream", "ByteArrayOutputStream", "FileInputStream",
		"FileOutputStream", "FileNotFoundException", "File", "Serializable",
		"Closeable", "Flushable",
	} {
		javaOf[n] = "java.io." + n
	}
	for _, n := range []string{
		"ZipEntry", "ZipFile", "ZipInputStream", "ZipOutputStream", "ZipException",
		"DataFormatException", "Adler32", "CRC32", "Checksum", "Deflater",
		"Inflater", "GZIPInputStream", "GZIPOutputStream",
	} {
		javaOf[n] = "java.util.zip." + n
	}
	for _, n := range []string{
		"MessageDigest", "DigestException", "GeneralSecurityException",
		"NoSuchAlgorithmException",
	} {
		javaOf[n] = "java.security." + n
	}
	for _, n := range []string{"Files", "Path", "Paths", "NoSuchFileException"} {
		javaOf[n] = "java.nio.file." + n
	}
	for _, n := range []string{
		"Executor", "ExecutorService", "Callable", "Future", "Executors",
		"FutureTask", "ConcurrentHashMap", "CountDownLatch", "TimeUnit",
		"RejectedExecutionException", "ExecutionException", "CancellationException",
		"ConcurrentMap", "CopyOnWriteArrayList", "BlockingQueue",
		"LinkedBlockingQueue", "ArrayBlockingQueue", "Semaphore", "ThreadFactory",
	} {
		javaOf[n] = "java.util.concurrent." + n
	}
	for _, n := range []string{"AtomicInteger", "AtomicLong", "AtomicBoolean", "AtomicReference"} {
		javaOf[n] = "java.util.concurrent.atomic." + n
	}
	for _, n := range []string{"Field", "Method", "Constructor", "Modifier", "Parameter"} {
		javaOf[n] = "java.lang.reflect." + n
	}

	// The standard library that Java has no class for, by layer: the reason
	// names the layer, because that is the decision a reader has to make.
	web := "the web framework is Teyru's: java.* has no HttpServer, Router or Application"
	json := "the JSON layer is Teyru's: java.* has no JsonElement, and this is not Gson's API"
	gsonshape := "the Gson-shaped layer is Teyru's, not com.google.gson"
	netshape := "the socket layer is Teyru's: java.net has a different API"
	tlsshape := "the TLS layer is Teyru's: javax.net.ssl has a different API"
	container := "the dependency-injection container is Teyru's: there is no java.* Spring"
	logging := "the logging layer is Teyru's: java.util.logging has a different API"
	reflect := "the reflection layer is Teyru's: java.lang.reflect has a different API"
	streams := "the stream helpers are Teyru's: java.util.stream has a different API"
	utilx := "the class is Teyru's; java.* has a class of the same name but not the same API"
	validation := "the validation layer is Teyru's: java.* has no validation API"
	for _, n := range []string{
		"HttpServer", "Http", "HttpClient", "HttpClientRequest", "HttpClientResponse",
		"HttpParser", "HttpRequest", "HttpResponse", "HttpSession", "HttpStatus",
		"HttpUrl", "Router", "Route", "RouteHandler", "RouteParams", "RouteResponse",
		"RouteScanner", "Application", "ApplicationContext", "StaticFiles", "WebUtil",
		"WebConvert", "WebAdvice", "Handler", "HandlerInterceptor", "MockServer",
		"MultipartFile", "MultipartForm", "ServerTask", "SpringApplication",
		"WebSocketApi", "WebSocketConnection", "WebSocketFrame", "WebSocketScanner",
		"WebSocketSession", "WebSocketSessionText", "WebSocketUpgrade",
		"WebSocketHandler", "Sessions",
	} {
		refuseStdlib[n] = web
	}
	for _, n := range []string{
		"JsonElement", "JsonObject", "JsonArray", "JsonPrimitive", "JsonNull",
		"JsonParser", "JsonReader", "JsonWriter", "JsonMember", "JsonMembers",
		"JsonNumbers", "JsonLazyNumber", "JsonParseException", "JsonSyntaxException",
		"JsonAdapterList", "JsonBinding", "JsonConvert", "JsonFields", "FieldNamingPolicy",
		"JsonReader2", "JsonWriter2", "JsonDeserializer", "JsonSerializer",
		"JsonSerializationContext", "JsonDeserializationContext", "BindingContext",
	} {
		refuseStdlib[n] = json
	}
	for _, n := range []string{"Gson", "GsonBuilder", "AdapterReader", "AdapterWriter", "ReflectReader", "ReflectWriter"} {
		refuseStdlib[n] = gsonshape
	}
	for _, n := range []string{
		"Net", "Socket", "ServerSocket", "SocketInputStream", "SocketOutputStream",
		"SocketException", "SocketTimeoutException", "UnknownHostException",
		"ClientResponseParser",
	} {
		refuseStdlib[n] = netshape
	}
	for _, n := range []string{
		"Tls", "TlsSocket", "TlsServer", "TlsException", "TlsCertificateException",
		"TlsConfigException", "TlsHandshakeException", "TlsHostnameException",
	} {
		refuseStdlib[n] = tlsshape
	}
	for _, n := range []string{
		"BeanDefinition", "BeanRegistry", "BeanScanner", "BeanFactory", "BeanInjector",
		"ReflectFactory", "ReflectInjector", "Singleton", "Component",
	} {
		refuseStdlib[n] = container
	}
	refuseStdlib["Logger"] = logging
	refuseStdlib["Log"] = logging
	for _, n := range []string{"Reflect", "Annotation", "Array"} {
		if _, isJava := javaOf[n]; !isJava {
			refuseStdlib[n] = reflect
		}
	}
	for _, n := range []string{
		"Streams", "StreamImpl", "StreamState", "StreamBox", "StreamBuilder",
		"Iterate", "CollectionSupport", "SetSupport", "ArrayOps", "Cont",
	} {
		refuseStdlib[n] = streams
	}
	for _, n := range []string{
		"Validation", "ValidationException", "TypeMismatchException",
	} {
		refuseStdlib[n] = validation
	}
	for _, n := range []string{
		"ZipFields", "ZipEntryStream", "Gzip", "ModifiedUtf8", "PosixTz",
		"TzifBlock", "TzifData",
		"TzMath", "TimeMath", "DateFields", "CharSet", "NumberDigits",
		"RegexSyntax", "PatternParser", "LineScanner", "Fs", "PropertiesFile",
		"Properties", "Sha1", "DecimalBig", "HttpSession",
	} {
		if _, isJava := javaOf[n]; !isJava {
			refuseStdlib[n] = utilx
		}
	}
	for _, n := range []string{
		"HashMapEntrySet", "HashMapIterator", "LinkedHashMapIterator",
		"TreeEntryIterator", "TreeEntrySet", "TreeKeySet", "TreeMapEntry",
		"TreeMapView", "SimpleEntry", "SnapshotEntry", "HashEntry",
		"MapKeyIterator", "MapKeySet", "MapValueIterator", "MapValueListIterator",
		"MapValues", "ArrayListIterator", "LinkedListIterator",
		"LinkedListDescendingIterator", "ArrayDequeIterator",
		"ArrayDequeDescendingIterator", "ListSubList", "ListSubListIterator",
		"SingletonList", "SingletonListIterator", "NCopiesList", "NCopiesIterator",
		"EmptyIterator", "ArrayAsList", "ArrayAsListIterator", "UnmodifiableList",
		"UnmodifiableMap", "UnmodifiableSet", "UnmodifiableCollection",
		"NaturalOrderComparator", "ReverseComparator", "SetEnumeration",
		"SpliteratorIterator", "SpliteratorSink", "IteratorSpliterator",
		"StreamFilterIterator", "StreamMapIterator", "StreamFlatMapIterator",
		"StreamDistinctIterator", "StreamSortedIterator", "StreamPeekIterator",
		"StreamLimitIterator", "StreamSkipIterator", "StreamTakeWhileIterator",
		"StreamDropWhileIterator", "StreamIterateIterator",
		"StreamIterateWhileIterator", "StreamGenerateIterator",
		"StreamConcatIterator", "CollectorChars", "CollectorImpl",
		"MessageElement", "LinkedNode", "MatchState",
	} {
		if _, isJava := javaOf[n]; !isJava {
			refuseStdlib[n] = utilx
		}
	}
	for _, n := range []string{
		"AverageAccumulator", "DoubleAverageAccumulator", "DoubleAccumulator",
		"IntAccumulator", "LongAccumulator", "IntSummaryStatistics",
		"LongSummaryStatistics", "DoubleSummaryStatistics",
	} {
		refuseStdlib[n] = utilx
	}
	refuseStdlib["IO"] = utilx
}

// annotationReasons classifies the annotations that are Teyru-only. An
// annotation not listed here is printed as written: Java's own (@Override,
// @Deprecated, @SafeVarargs, ...) and the program's own go through unchanged.
var annotationReasons = map[string]string{}

func init() {
	lombok := "Lombok's generated members are a Teyru compatibility layer; write them out as Java"
	for _, n := range []string{
		"Data", "Getter", "Setter", "Builder", "SuperBuilder", "ToString",
		"EqualsAndHashCode", "Value", "NonNull", "AllArgsConstructor",
		"NoArgsConstructor", "RequiredArgsConstructor", "Singular", "Tolerate",
		"SneakyThrows", "Synchronized", "Log", "Slf4j", "CommonsLog", "Log4j",
		"Log4j2", "XSlf4j", "Flogger", "JBossLog", "CustomLog", "UtilityClass",
		"FieldNameConstants", "ExtensionMethod", "Delegate", "Accessors",
		"Cleanup", "StandardException", "Helper", "Since", "Until", "With",
		"PackagePrivate", "NonFinal", "Locked", "Var", "Jacksonized",
		"FieldDefaults", "Include", "Exclude", "OnMethod_", "OnParam_",
	} {
		annotationReasons[n] = lombok
	}
	spring := "the Spring-shaped framework is Teyru's, not a java.* API"
	for _, n := range []string{
		"RestController", "Controller", "Service", "Component", "Repository",
		"Configuration", "Bean", "Autowired", "RequestMapping", "GetMapping",
		"PostMapping", "PutMapping", "DeleteMapping", "PatchMapping", "RequestBody",
		"RequestParam", "RequestHeader", "PathVariable", "Qualifier", "Primary",
		"Profile", "Scope", "PostConstruct", "PreDestroy", "SpringBootApplication",
		"ConfigurationProperties", "ControllerAdvice", "ExceptionHandler",
		"ResponseBody", "CrossOrigin", "WebSocketMapping",
	} {
		annotationReasons[n] = spring
	}
	gson := "the Gson-shaped JSON layer is Teyru's, not com.google.gson's annotations"
	for _, n := range []string{"SerializedName", "Expose", "JsonAdapter"} {
		annotationReasons[n] = gson
	}
	for _, n := range []string{"NotNull", "Min", "Max", "Size", "Pattern", "Email", "Positive"} {
		if _, ok := annotationReasons[n]; !ok {
			annotationReasons[n] = "the validation layer is Teyru's: java.* has no validation API"
		}
	}
}

// importReasons classifies the imports that bring in a layer Java has no
// equivalent of, so that a program importing one is refused with the layer's
// name rather than with a missing class much later.
var importReasons = map[string]string{}

func init() {
	lombok := "Lombok's generated members are a Teyru compatibility layer; write them out as Java"
	importReasons["lombok"] = lombok
	gson := "the Gson-shaped JSON layer is Teyru's, not com.google.gson"
	importReasons["com.google.gson"] = gson
	importReasons["module"] = "a module import (JEP 511) needs Java 25; the reference JDK here is 21"
}

// pathReason classifies an import path by its first segment.
func pathReason(path string) (string, bool) {
	if r, ok := importReasons[path]; ok {
		return r, true
	}
	if i := strings.IndexByte(path, '.'); i > 0 {
		r, ok := importReasons[path[:i]]
		return r, ok
	}
	return "", false
}

// names returns the keys of a string map, sorted, for a stable report.
func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
