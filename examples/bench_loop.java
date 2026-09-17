public class bench_loop {
  public static void main(String[] args) {
    int rounds = 20000;
    if (args.length > 0) { rounds = Integer.parseInt(args[0]); }
    int[] xs = new int[1024];
    for (int i = 0; i < 1024; i++) { xs[i] = i; }
    long total = 0;
    for (int round = 0; round < rounds; round++) {
      for (int i = 0; i < 1024; i++) { total += xs[i] % 7; }
    }
    System.out.println(total);
  }
}
