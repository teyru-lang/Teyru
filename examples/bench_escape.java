class EscCell {
  private long value;
  public EscCell(long v) { value = v; }
  public long value() { return value; }
}

public class bench_escape {
  private static int ringSize = 1024;
  private static EscCell[] ring = new EscCell[ringSize];

  private static EscCell make(long v) { return new EscCell(v); }

  public static void main(String[] args) {
    int n = 20000000;
    if (args.length > 0) { n = Integer.parseInt(args[0]); }
    int mask = ringSize - 1;
    long sum = 0;
    for (int i = 0; i < n; i++) {
      EscCell c = make(i);
      ring[i & mask] = c;
      sum += ring[i & mask].value();
    }
    System.out.println(sum);
  }
}
