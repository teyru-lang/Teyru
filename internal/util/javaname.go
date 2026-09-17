// Java names for the standard library's classes.
//
// The standard library is a copy of Java's: teyru.ArrayList is java.util.ArrayList
// with the same API, teyru.String is java.lang.String, and the exception classes
// are java.lang's. Two things need that mapping and they need the same one, so it
// is here rather than beside either of them. The Java printer (internal/java) asks
// it what an import has to name; the code generator asks it what a class reports,
// because decision D8 of the fix plan is that Class.getName, and every message
// built out of it, answers with the JDK's name rather than with teyru.*.
//
// The classes that are Teyru's own -- the web framework, the JSON layer, the
// socket layer, the reflection layer under a different API -- are not here at
// all, and a name that is not here is reported as the class is really called.

package util

import "strings"

// libPrefix is the package every standard-library class is declared in.
const libPrefix = "teyru."

// javaClasses maps a standard-library class name to the JDK class it stands in
// for, keyed by the name as the source writes it (the simple name: every class
// of the standard library is in package teyru, so simple names are unique).
var javaClasses = map[string]string{}

func init() {
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
		javaClasses[n] = "java.lang." + n
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
		javaClasses[n] = "java.util." + n
	}
	for _, n := range []string{
		"NumberFormat", "DecimalFormat", "DateFormat", "SimpleDateFormat",
		"MessageFormat", "DateFormatSymbols", "DecimalFormatSymbols",
		"ParsePosition", "ChoiceFormat", "Format", "ParseException",
	} {
		javaClasses[n] = "java.text." + n
	}
	javaClasses["Spliterators"] = "java.util.Spliterators"
	javaClasses["Random"] = "java.util.Random"
	for _, n := range []string{
		"Stream", "IntStream", "LongStream", "DoubleStream", "Collector", "Collectors",
	} {
		javaClasses[n] = "java.util.stream." + n
	}
	javaClasses["StreamSupport"] = "java.util.stream.StreamSupport"
	for _, n := range []string{
		"Function", "BiFunction", "Consumer", "Supplier", "Predicate",
		"UnaryOperator", "BinaryOperator", "BiConsumer", "BiPredicate",
	} {
		javaClasses[n] = "java.util.function." + n
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
		javaClasses[n] = "java.util.function." + n
	}
	for _, n := range []string{"Pattern", "Matcher", "MatchResult", "PatternSyntaxException"} {
		javaClasses[n] = "java.util.regex." + n
	}
	for _, n := range []string{"BigInteger", "BigDecimal", "MathContext", "RoundingMode"} {
		javaClasses[n] = "java.math." + n
	}
	for _, n := range []string{
		"LocalDate", "LocalTime", "LocalDateTime", "Instant", "Duration", "Period",
		"ZoneId", "ZoneOffset", "ZonedDateTime", "DayOfWeek", "Month",
		"DateTimeException", "UnsupportedTemporalTypeException", "Clock",
		"MonthDay", "YearMonth", "Year", "OffsetDateTime", "OffsetTime",
	} {
		javaClasses[n] = "java.time." + n
	}
	for _, n := range []string{"DateTimeFormatter", "DateTimeParseException", "FormatStyle", "ResolverStyle"} {
		javaClasses[n] = "java.time.format." + n
	}
	for _, n := range []string{"ZoneOffsetTransition", "ZoneRules", "ZoneRulesException"} {
		javaClasses[n] = "java.time.zone." + n
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
		javaClasses[n] = "java.io." + n
	}
	for _, n := range []string{
		"ZipEntry", "ZipFile", "ZipInputStream", "ZipOutputStream", "ZipException",
		"DataFormatException", "Adler32", "CRC32", "Checksum", "Deflater",
		"Inflater", "GZIPInputStream", "GZIPOutputStream",
	} {
		javaClasses[n] = "java.util.zip." + n
	}
	for _, n := range []string{
		"MessageDigest", "DigestException", "GeneralSecurityException",
		"NoSuchAlgorithmException",
	} {
		javaClasses[n] = "java.security." + n
	}
	for _, n := range []string{"Files", "Path", "Paths", "NoSuchFileException"} {
		javaClasses[n] = "java.nio.file." + n
	}
	for _, n := range []string{
		"Executor", "ExecutorService", "Callable", "Future", "Executors",
		"FutureTask", "ConcurrentHashMap", "CountDownLatch", "TimeUnit",
		"RejectedExecutionException", "ExecutionException", "CancellationException",
		"ConcurrentMap", "CopyOnWriteArrayList", "BlockingQueue",
		"LinkedBlockingQueue", "ArrayBlockingQueue", "Semaphore", "ThreadFactory",
	} {
		javaClasses[n] = "java.util.concurrent." + n
	}
	for _, n := range []string{"AtomicInteger", "AtomicLong", "AtomicBoolean", "AtomicReference"} {
		javaClasses[n] = "java.util.concurrent.atomic." + n
	}
	for _, n := range []string{"Field", "Method", "Constructor", "Modifier", "Parameter"} {
		javaClasses[n] = "java.lang.reflect." + n
	}
}

// JavaClassOf answers the JDK class a standard-library class of this name stands
// in for, and false for a class Java has no counterpart of. It is keyed by the
// simple name a program writes.
func JavaClassOf(name string) (string, bool) {
	fqn, ok := javaClasses[name]
	return fqn, ok
}

// JavaName answers the name a class reports: the JDK name of the class it stands
// in for, or the class's own binary name when it is one of Teyru's own. A nested
// class keeps its nesting under the outer class's JDK name, so teyru.Map$Entry
// reports java.util.Map$Entry.
//
// This is the name Class.getName answers with, which is why the identity of a
// class is still its binary name: Class.forName looks a class up by that one
// (teyru.String and java.lang.String both find the string class, see
// lib/26_reflect.teyru), while what a program *prints* is the JDK's name.
func JavaName(full string) string {
	outer, nested := full, ""
	if i := strings.IndexByte(full, '$'); i >= 0 {
		outer, nested = full[:i], full[i:]
	}
	if !strings.HasPrefix(outer, libPrefix) {
		return full
	}
	fqn, ok := javaClasses[outer[len(libPrefix):]]
	if !ok {
		return full
	}
	return fqn + nested
}
