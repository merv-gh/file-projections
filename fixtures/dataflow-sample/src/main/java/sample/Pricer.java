package sample;

public class Pricer {
    public String price(int amount) {
        int fee = 5;
        int discount = amount / 10;
        int net = amount - discount;
        int total = fee;
        return "$" + total;
    }
}
