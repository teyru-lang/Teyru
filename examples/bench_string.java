public class bench_string {
  public static void main(String[] args) {
    int n = 200000;
    if (args.length > 0) { n = Integer.parseInt(args[0]); }
    String base = "teyru";
    long total = 0;
    for (int i = 0; i < n; i++) {
      String s = base + "-" + i;
      total += s.length();
    }
    System.out.println(total);
  }
}
