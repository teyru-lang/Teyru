class Cell {
  private long value;
  public Cell(long v) { value = v; }
  public long value() { return value; }
}

public class bench_alloc {
  public static void main(String[] args) {
    int n = 20000000;
    if (args.length > 0) { n = Integer.parseInt(args[0]); }
    long sum = 0;
    for (int i = 0; i < n; i++) {
      Cell c = new Cell(i);
      sum += c.value();
    }
    System.out.println(sum);
  }
}
