import java.lang.reflect.*;

class Adder {
  private int base;
  public Adder(int base) { this.base = base; }
  public int add(int x) { return base + x; }
}

public class bench_invoke {
  public static void main(String[] args) throws Exception {
    int rounds = 20000000;
    if (args.length > 0) { rounds = Integer.parseInt(args[0]); }
    Adder a = new Adder(7);
    Method add = Adder.class.getDeclaredMethod("add", int.class);
    Object[] box = new Object[1];

    long direct = 0;
    long t0 = System.nanoTime();
    for (int i = 0; i < rounds; i++) { direct += a.add(i); }
    long t1 = System.nanoTime();

    long reflected = 0;
    for (int i = 0; i < rounds; i++) {
      box[0] = i;
      reflected += (Integer) add.invoke(a, box);
    }
    long t2 = System.nanoTime();

    System.out.println("direct " + (t1 - t0) / 1000000 + " ms");
    System.out.println("invoke " + (t2 - t1) / 1000000 + " ms");
    System.out.println(direct == reflected);
  }
}
